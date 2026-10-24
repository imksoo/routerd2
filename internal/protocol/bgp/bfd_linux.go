//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package bgp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	gobgpapi "github.com/osrg/gobgp/v4/api"
)

const BFDControlFreshness = 500 * time.Millisecond
const bfdAdminTimeout = 500 * time.Millisecond

// BFDUpdate is local transport metadata, not the control API's JSON document.
// Received is stamped when the HTTP response completes, before any queueing.
// Failures is a cumulative counter so latest-value coalescing retains a failed
// control request even if the following request succeeds before consumption.
type BFDUpdate struct {
	Projection bgpconfig.BFDProjection
	Received   bfd.Clock
	Available  bool
	Failures   uint64
}

type bfdInput struct {
	digest     string
	generation uint64
	updates    <-chan BFDUpdate
}

// RunWithBFD starts all bound peers administratively down, then consumes
// finite, generation-scoped BFD health independently of route effects. An
// absent or stopped input stream never falls back to ordinary ungated BGP.
func RunWithBFD(ctx context.Context, cfg Config, digest string, generation uint64, updates <-chan BFDUpdate, effects Effects, ready func()) error {
	if len(cfg.PeerBFD) == 0 || updates == nil {
		return errors.New("BFD-gated BGP requires explicit references and an input stream")
	}
	if err := bgpconfig.ValidateBFDProjection(cfg, bgpconfig.BFDProjection{ConfigDigest: digest, Generation: generation}, digest, generation); err != nil {
		return err
	}
	return runBounded(ctx, cfg, effects, ready, &bfdInput{digest: digest, generation: generation, updates: updates})
}

type peerAdminAPI interface {
	EnablePeer(context.Context, *gobgpapi.EnablePeerRequest) error
	DisablePeer(context.Context, *gobgpapi.DisablePeerRequest) error
	ListPeer(context.Context, *gobgpapi.ListPeerRequest, func(*gobgpapi.Peer)) error
}

type bfdSource struct {
	digest      string
	generation  uint64
	epoch, boot string
}

func sourceOf(g bgpconfig.BFDGrant) bfdSource {
	return bfdSource{g.ConfigDigest, g.Generation, g.ProducerEpoch, g.BootID}
}
func completeSource(g bgpconfig.BFDGrant) bool {
	return g.ConfigDigest != "" && g.Generation != 0 && g.ProducerEpoch != "" && g.Sequence != 0 && g.BootID != ""
}

type bfdPeerGate struct {
	address                               string
	grant                                 bgpconfig.BFDGrant
	source                                bfdSource
	seen, present                         bool
	retired                               map[bfdSource]bool
	minReceive                            int64
	forceReset, wanted, enabled, withheld bool
	needsWithdrawal                       bool
	withdrawalVersion                     uint64
	operation                             string
	operationStarted                      int64
	operationDeadline                     int64
	operationCancel                       context.CancelFunc
	acknowledged, pendingProof            bgpconfig.BFDAcknowledgement
}

type bfdGateView struct {
	mu                   sync.RWMutex
	entries              map[string]bfdGateEntry
	disabledAcknowledged map[string]uint64
	changed              func()
}

type bfdGateState struct {
	proof                                 bgpconfig.BFDAcknowledgement
	source                                bfdSource
	downTransitions                       uint64
	forceReset, wanted, enabled, withheld bool
	needsWithdrawal                       bool
	operation                             string
}

type bfdGateEntry struct {
	state   bfdGateState
	version uint64
}

func (v *bfdGateView) publish(peers map[string]*bfdPeerGate) {
	v.mu.Lock()
	changed := false
	if v.entries == nil {
		v.entries = make(map[string]bfdGateEntry, len(peers))
	}
	for address, g := range peers {
		proof := g.acknowledged
		proof.Enabled = g.enabled && g.wanted && !g.forceReset && g.operation == "" && !g.withheld
		state := bfdGateState{proof: proof, source: g.source, downTransitions: g.grant.DownTransitions, forceReset: g.forceReset, wanted: g.wanted, enabled: g.enabled, withheld: g.withheld, needsWithdrawal: g.needsWithdrawal, operation: g.operation}
		entry, present := v.entries[address]
		if !present || entry.state != state {
			entry.state = state
			if entry.version < math.MaxUint64 {
				entry.version++
			}
			v.entries[address] = entry
			changed = true
		}
		if g.needsWithdrawal && g.withdrawalVersion == 0 {
			// Bind this reset to the first published disabled gate state.
			// Acknowledgments of older in-flight RIB snapshots cannot satisfy it.
			g.withdrawalVersion = entry.version
		}
	}
	v.mu.Unlock()
	if changed && v.changed != nil {
		v.changed()
	}
}

func (v *bfdGateView) snapshotWithVersion() map[string]bfdGateEntry {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.entries == nil {
		return nil
	}
	out := make(map[string]bfdGateEntry, len(v.entries))
	for address, entry := range v.entries {
		out[address] = entry
	}
	return out
}

func (v *bfdGateView) snapshot() map[string]bgpconfig.BFDAcknowledgement {
	entries := v.snapshotWithVersion()
	if entries == nil {
		return nil
	}
	out := make(map[string]bgpconfig.BFDAcknowledgement, len(entries))
	for address, entry := range entries {
		proof := entry.state.proof
		if entry.version == math.MaxUint64 {
			proof.Enabled = false
		}
		out[address] = proof
	}
	return out
}

// A RIB walk can straddle a complete disable/enable cycle. Its old paths must
// never inherit a newer enable proof. Per-peer versions retain such an ABA
// transition even when source identity and DownTransitions are unchanged.
// Ordinary packet refreshes do not advance the version or starve slow walks.
func (v *bfdGateView) readSnapshot(read func() (Snapshot, error)) (Snapshot, map[string]uint64, error) {
	before := v.snapshotWithVersion()
	snapshot, err := read()
	if err != nil {
		return snapshot, nil, err
	}
	after := v.snapshotWithVersion()
	changed := make(map[string]bool)
	disabled := make(map[string]uint64)
	if after != nil {
		snapshot.BFD = make(map[string]bgpconfig.BFDAcknowledgement, len(after))
	}
	for address, entry := range after {
		prior, present := before[address]
		proof := entry.state.proof
		if !present || prior.version != entry.version || entry.version == math.MaxUint64 {
			changed[address] = true
			proof.Enabled = false
		}
		snapshot.BFD[address] = proof
		if !proof.Enabled {
			disabled[address] = entry.version
		}
	}
	for address := range before {
		if _, present := after[address]; !present {
			changed[address] = true
		}
	}
	if len(changed) != 0 {
		routes := make([]Route, 0, len(snapshot.Routes))
		for _, route := range snapshot.Routes {
			if !changed[route.PeerAddress] {
				routes = append(routes, route)
			}
		}
		snapshot.Routes = routes
	}
	return snapshot, disabled, nil
}

// Only a successful ownership Effects acknowledgment can release a reset.
// The publication carries the serial captured around its own RIB walk, never
// whichever gate serial happens to be current when the slow callback returns.
func (v *bfdGateView) acknowledge(disabled map[string]uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.disabledAcknowledged == nil {
		v.disabledAcknowledged = make(map[string]uint64)
	}
	for address, version := range disabled {
		if entry, present := v.entries[address]; present && version <= entry.version && version > v.disabledAcknowledged[address] {
			v.disabledAcknowledged[address] = version
		}
	}
}

func (v *bfdGateView) withdrawalAccepted(address string, version uint64) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return version > 0 && version < math.MaxUint64 && v.disabledAcknowledged[address] >= version
}

func (g *bfdPeerGate) block(now int64) {
	if g.seen || g.enabled || g.operation == "enable" {
		g.forceReset = true
		g.minReceive = max(g.minReceive, now)
	}
}

func (g *bfdPeerGate) observe(grant bgpconfig.BFDGrant, present bool, now int64) error {
	g.present = false
	if !present {
		if g.wanted || g.enabled || g.operation == "enable" {
			g.block(now)
		}
		return nil
	}
	if !completeSource(grant) {
		if grant.Up {
			return errors.New("BFD Up has no complete source identity")
		}
		if g.wanted || g.enabled || g.operation == "enable" {
			g.block(now)
		}
		return nil
	}
	next := sourceOf(grant)
	if g.seen && next != g.source {
		if g.retired[next] {
			return errors.New("retired BFD source epoch reappeared")
		}
		if len(g.retired) >= 128 {
			return errors.New("BFD source epoch history is full; restart BGP worker")
		}
		g.retired[g.source] = true
		g.block(now)
	} else if g.seen {
		if grant.Sequence < g.grant.Sequence || grant.DownTransitions < g.grant.DownTransitions || grant.LastReceivedBootNanoseconds < g.grant.LastReceivedBootNanoseconds {
			return errors.New("BFD source sequence, transitions or receive evidence regressed")
		}
		if grant.Sequence == g.grant.Sequence && grant.Up && grant != g.grant {
			return errors.New("BFD source sequence was reused for changed Up evidence")
		}
		if grant.DownTransitions != g.grant.DownTransitions {
			g.block(now)
		}
	}
	if !grant.Up && (g.wanted || g.enabled || g.operation == "enable") {
		g.block(now)
	}
	g.seen, g.present, g.source, g.grant = true, true, next, grant
	return nil
}

func (g *bfdPeerGate) healthy(clock bfd.Clock, control bool) bool {
	return !g.withheld && control && g.seen && g.present && g.grant.Up && g.grant.BootID == clock.BootID && g.grant.LastReceivedBootNanoseconds > g.minReceive && g.grant.LastReceivedBootNanoseconds <= clock.BoottimeNanoseconds && clock.BoottimeNanoseconds < g.grant.DeadlineBootNanoseconds
}

type bfdAdminResult struct {
	address, operation string
	err                error
	received           bfd.Clock
}

func (g *bfdPeerGate) completeAdmin(result bfdAdminResult, clock bfd.Clock) error {
	if g.address != result.address || g.operation != result.operation {
		return errors.New("BGP BFD admin acknowledgment lost its operation identity")
	}
	if result.err != nil {
		return fmt.Errorf("BGP BFD %s acknowledgment for %s: %w", result.operation, result.address, result.err)
	}
	// Judge the observed ACK, not how long its buffered delivery waited for
	// this consumer to run. The receive deadline for recovery still uses now.
	received := result.received.BoottimeNanoseconds
	if result.received.BootID != clock.BootID || received <= 0 || received < g.operationStarted || received > clock.BoottimeNanoseconds || received >= g.operationDeadline {
		return fmt.Errorf("BGP BFD %s acknowledgment outside its deadline for %s (received=%d deadline=%d processing=%d); worker process exit is required", g.operation, result.address, received, g.operationDeadline, clock.BoottimeNanoseconds)
	}
	g.operationCancel()
	g.operationCancel = nil
	g.operation, g.operationStarted, g.operationDeadline = "", 0, 0
	g.enabled = result.operation == "enable"
	if g.enabled {
		g.acknowledged = g.pendingProof
	}
	if result.operation == "disable" {
		g.forceReset = false
		g.minReceive = max(g.minReceive, clock.BoottimeNanoseconds)
	}
	return nil
}

// GoBGP Enable/Disable only enqueue an adminStateCh message; a full channel can
// even return success without enqueuing it. Never invert that request until the
// exact peer has acknowledged its state. DOWN+IDLE follows the old TCP close.
func acknowledgedAdmin(ctx context.Context, server peerAdminAPI, address, operation string) error {
	var err error
	switch operation {
	case "enable":
		err = server.EnablePeer(ctx, &gobgpapi.EnablePeerRequest{Address: address})
	case "disable":
		err = server.DisablePeer(ctx, &gobgpapi.DisablePeerRequest{Address: address, Communication: "routerd BFD health unavailable"})
	}
	if err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		found, acknowledged := false, false
		err := server.ListPeer(ctx, &gobgpapi.ListPeerRequest{Address: address}, func(p *gobgpapi.Peer) {
			if p.GetConf().GetNeighborAddress() != address {
				return
			}
			found = true
			state := p.GetState()
			if operation == "enable" {
				acknowledged = state.GetAdminState() == gobgpapi.PeerState_ADMIN_STATE_UP
			} else {
				acknowledged = state.GetAdminState() == gobgpapi.PeerState_ADMIN_STATE_DOWN && state.GetSessionState() == gobgpapi.PeerState_SESSION_STATE_IDLE
			}
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !found {
			return errors.New("BFD-bound BGP peer disappeared")
		}
		if acknowledged {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func startBFDAdmin(ctx context.Context, server peerAdminAPI, g *bfdPeerGate, operation string, now int64, results chan<- bfdAdminResult, readClock func() (bfd.Clock, error)) {
	call, cancel := context.WithTimeout(ctx, bfdAdminTimeout)
	g.operation, g.operationStarted, g.operationDeadline, g.operationCancel = operation, now, now+int64(bfdAdminTimeout), cancel
	if operation == "enable" {
		g.pendingProof = bgpconfig.BFDAcknowledgement{ConfigDigest: g.grant.ConfigDigest, Generation: g.grant.Generation, ProducerEpoch: g.grant.ProducerEpoch, DownTransitions: g.grant.DownTransitions}
	}
	go func() {
		err := acknowledgedAdmin(call, server, g.address, operation)
		received, clockErr := readClock()
		cancel()
		results <- bfdAdminResult{address: g.address, operation: operation, err: errors.Join(err, clockErr), received: received}
	}()
}

func runBFDGate(ctx context.Context, cfg Config, input bfdInput, server peerAdminAPI, withheld map[string]bool, view *bfdGateView, readClock func() (bfd.Clock, error)) error {
	peers := make(map[string]*bfdPeerGate, len(cfg.PeerBFD))
	order := make([]string, 0, len(cfg.PeerBFD))
	for address := range cfg.PeerBFD {
		peers[address] = &bfdPeerGate{address: address, retired: map[bfdSource]bool{}, withheld: withheld[address]}
		order = append(order, address)
	}
	sort.Strings(order)
	results := make(chan bfdAdminResult, len(peers))
	defer func() {
		for _, g := range peers {
			if g.operationCancel != nil {
				g.operationCancel()
			}
			g.wanted = false
		}
		view.publish(peers)
	}()
	clock, err := readClock()
	if err != nil {
		return err
	}
	bootID, previousNow := clock.BootID, clock.BoottimeNanoseconds
	for _, address := range order {
		if !peers[address].withheld {
			startBFDAdmin(ctx, server, peers[address], "initial", previousNow, results, readClock)
		}
	}
	var controlDeadline, receivedAt int64
	var failures uint64
	available := false
	var incoming *BFDUpdate
	var completed *bfdAdminResult
	completions := make([]bfdAdminResult, 0, len(peers))
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Ready ACKs take precedence over control-update traffic and the timeout
		// check. There is at most one outstanding operation per bound peer.
		completions = completions[:0]
		if completed != nil {
			completions = append(completions, *completed)
			completed = nil
		}
	drain:
		for len(completions) < len(peers) {
			select {
			case result := <-results:
				completions = append(completions, result)
			default:
				break drain
			}
		}
		clock, err = readClock()
		if err != nil {
			return err
		}
		now := clock.BoottimeNanoseconds
		if clock.BootID == "" || clock.BootID != bootID || now < previousNow || now <= 0 || now > math.MaxInt64-int64(BFDControlFreshness) {
			return errors.New("BGP BFD clock identity changed")
		}
		previousNow = now
		for _, result := range completions {
			g := peers[result.address]
			if g == nil {
				return errors.New("BGP BFD admin acknowledgment lost its peer")
			}
			if err := g.completeAdmin(result, clock); err != nil {
				return err
			}
		}
		if incoming != nil {
			update := *incoming
			incoming = nil
			if update.Received.BootID != bootID || update.Received.BoottimeNanoseconds <= 0 || update.Received.BoottimeNanoseconds < receivedAt || update.Received.BoottimeNanoseconds > now || update.Failures < failures {
				return errors.New("BGP BFD control receipt identity or failure counter regressed")
			}
			if update.Available {
				if err := bgpconfig.ValidateBFDProjection(cfg, update.Projection, input.digest, input.generation); err != nil {
					return err
				}
				for _, grant := range update.Projection.Peers {
					if grant.BootID == bootID && grant.LastReceivedBootNanoseconds > update.Received.BoottimeNanoseconds {
						return errors.New("BFD grant receive evidence is newer than its control receipt")
					}
				}
			}
			if update.Failures != failures {
				for _, g := range peers {
					g.block(now)
				}
			}
			receivedAt, failures, available = update.Received.BoottimeNanoseconds, update.Failures, update.Available
			controlDeadline = receivedAt + int64(BFDControlFreshness)
			for _, address := range order {
				grant, present := update.Projection.Peers[address]
				if err := peers[address].observe(grant, available && present, now); err != nil {
					return fmt.Errorf("BGP peer %s: %w", address, err)
				}
			}
		}
		control := available && now < controlDeadline
		next := now + int64(10*time.Millisecond)
		if controlDeadline > now {
			next = min(next, controlDeadline)
		}
		for _, address := range order {
			g := peers[address]
			if g.operation != "" && now >= g.operationDeadline {
				return fmt.Errorf("BGP BFD %s acknowledgment timed out for %s; worker process exit is required", g.operation, address)
			}
			healthy := g.healthy(clock, control)
			if !healthy && g.wanted {
				g.block(now)
			}
			g.wanted = healthy
			if g.operation == "" && !g.withheld {
				switch {
				case g.enabled && (!healthy || g.forceReset):
					g.needsWithdrawal = true
					g.withdrawalVersion = 0
					startBFDAdmin(ctx, server, g, "disable", now, results, readClock)
				case !g.enabled && g.forceReset:
					g.forceReset = false
					g.minReceive = max(g.minReceive, now)
				case !g.enabled && healthy && (!g.needsWithdrawal || view.withdrawalAccepted(address, g.withdrawalVersion)):
					g.needsWithdrawal = false
					g.withdrawalVersion = 0
					startBFDAdmin(ctx, server, g, "enable", now, results, readClock)
				}
			}
			if g.operationDeadline > now {
				next = min(next, g.operationDeadline)
			}
			if g.grant.Up && g.grant.DeadlineBootNanoseconds > now {
				next = min(next, g.grant.DeadlineBootNanoseconds)
			}
		}
		view.publish(peers)
		timer := time.NewTimer(time.Duration(next - now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case update, open := <-input.updates:
			timer.Stop()
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("BGP BFD control input closed")
			}
			incoming = &update
		case result := <-results:
			timer.Stop()
			completed = &result
		case <-timer.C:
		}
	}
}
