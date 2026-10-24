//go:build linux

package bgp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	api "github.com/osrg/gobgp/v4/api"
)

const gatePeer = "192.0.2.2"
const gateBoot = "01234567-89ab-cdef-0123-456789abcdef"

func gateGrant(at int64, sequence, down uint64) bgpconfig.BFDGrant {
	return bgpconfig.BFDGrant{Service: "health", Address: gatePeer, ConfigDigest: strings.Repeat("b", 64), Generation: 7, ProducerEpoch: strings.Repeat("c", 32), Sequence: sequence, BootID: gateBoot, LastReceivedBootNanoseconds: at, DeadlineBootNanoseconds: at + int64(time.Second), DownTransitions: down, Up: true}
}

func gateConfig() Config {
	return Config{Runtime: bgpdaemon.AppliedConfig{Peers: map[string]bgpdaemon.AppliedPeer{gatePeer: {Address: gatePeer, ASN: 64513}}}, PeerBFD: map[string]bgpconfig.BFDReference{gatePeer: {Service: "health", Address: gatePeer}}}
}

func TestBFDGateAuthoritativeEvidenceSurvivesTransientControlFailure(t *testing.T) {
	g := &bfdPeerGate{address: gatePeer, retired: map[bfdSource]bool{}}
	grant := gateGrant(int64(time.Second), 1, 0)
	if err := g.observe(grant, true, int64(time.Second)); err != nil {
		t.Fatal(err)
	}
	g.enabled, g.wanted = true, true
	if err := g.observe(bgpconfig.BFDGrant{}, false, int64(time.Second+10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if g.grant != grant || g.present {
		t.Fatal("control loss mutated authoritative source evidence")
	}
	if err := g.observe(grant, true, int64(time.Second+20*time.Millisecond)); err != nil {
		t.Fatal("unchanged sequence replay after failure was misclassified:", err)
	}
	if g.healthy(bfd.Clock{BootID: gateBoot, BoottimeNanoseconds: int64(time.Second + 20*time.Millisecond)}, true) || !g.forceReset {
		t.Fatal("replayed Up bypassed reset/fresh-packet fence")
	}
}

func TestBFDGateSourceReplayAndCoalescedLoss(t *testing.T) {
	for _, mode := range []string{"counter", "identity", "sequence", "receive", "retired"} {
		t.Run(mode, func(t *testing.T) {
			g := &bfdPeerGate{address: gatePeer, retired: map[bfdSource]bool{}}
			first := gateGrant(int64(time.Second), 3, 1)
			if err := g.observe(first, true, int64(time.Second)); err != nil {
				t.Fatal(err)
			}
			g.enabled, g.wanted = true, true
			next := gateGrant(int64(time.Second+10*time.Millisecond), 4, 1)
			switch mode {
			case "counter":
				next.DownTransitions = 2
			case "identity", "retired":
				next.ProducerEpoch = strings.Repeat("d", 32)
				next.Sequence = 1
				next.DownTransitions = 0
			case "sequence":
				next.Sequence = 2
			case "receive":
				next.LastReceivedBootNanoseconds--
				next.LastReceivedBootNanoseconds -= int64(20 * time.Millisecond)
			}
			err := g.observe(next, true, int64(time.Second+20*time.Millisecond))
			if mode == "sequence" || mode == "receive" {
				if err == nil {
					t.Fatal("source regression accepted")
				}
				return
			}
			if err != nil || !g.forceReset {
				t.Fatalf("coalesced/source failure did not latch reset: %v", err)
			}
			if mode == "retired" {
				if err := g.observe(first, true, int64(time.Second+30*time.Millisecond)); err == nil {
					t.Fatal("retired epoch was revived")
				}
			}
		})
	}
}

type fakeAdmin struct {
	mu                     sync.Mutex
	admin                  api.PeerState_AdminState
	state                  api.PeerState_SessionState
	commands               []string
	holdDown, ignoreEnable bool
	afterList              func(api.PeerState_AdminState)
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{admin: api.PeerState_ADMIN_STATE_DOWN, state: api.PeerState_SESSION_STATE_IDLE}
}
func (f *fakeAdmin) EnablePeer(_ context.Context, _ *api.EnablePeerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "enable")
	if !f.ignoreEnable {
		f.admin, f.state = api.PeerState_ADMIN_STATE_UP, api.PeerState_SESSION_STATE_ESTABLISHED
	}
	return nil
}
func (f *fakeAdmin) DisablePeer(_ context.Context, _ *api.DisablePeerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "disable")
	f.admin = api.PeerState_ADMIN_STATE_DOWN
	if !f.holdDown {
		f.state = api.PeerState_SESSION_STATE_IDLE
	}
	return nil
}
func (f *fakeAdmin) ListPeer(ctx context.Context, _ *api.ListPeerRequest, fn func(*api.Peer)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	p := &api.Peer{Conf: &api.PeerConf{NeighborAddress: gatePeer}, State: &api.PeerState{AdminState: f.admin, SessionState: f.state}}
	afterList := f.afterList
	f.mu.Unlock()
	fn(p)
	if afterList != nil {
		afterList(p.State.AdminState)
	}
	return nil
}
func (f *fakeAdmin) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

type gateHarness struct {
	clock     atomic.Int64
	readClock func() (bfd.Clock, error)
	updates   chan BFDUpdate
	admin     *fakeAdmin
	view      *bfdGateView
	done      chan error
	cancel    context.CancelFunc
}

func newGateHarness(t *testing.T) *gateHarness {
	return newGateHarnessPrepared(t, nil)
}

func newGateHarnessPrepared(t *testing.T, prepare func(*gateHarness)) *gateHarness {
	t.Helper()
	h := &gateHarness{updates: make(chan BFDUpdate, 8), admin: newFakeAdmin(), view: &bfdGateView{}, done: make(chan error, 1)}
	h.clock.Store(int64(time.Second))
	h.readClock = func() (bfd.Clock, error) {
		return bfd.Clock{BootID: gateBoot, BoottimeNanoseconds: h.clock.Load()}, nil
	}
	if prepare != nil {
		prepare(h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- runBFDGate(ctx, gateConfig(), bfdInput{digest: strings.Repeat("a", 64), generation: 9, updates: h.updates}, h.admin, nil, h.view, h.readClock)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("BFD gate did not cancel")
		}
	})
	return h
}
func (h *gateHarness) send(g bgpconfig.BFDGrant, available bool, failures uint64) {
	h.updates <- BFDUpdate{Projection: bgpconfig.BFDProjection{ConfigDigest: strings.Repeat("a", 64), Generation: 9, Peers: map[string]bgpconfig.BFDGrant{gatePeer: g}}, Received: bfd.Clock{BootID: gateBoot, BoottimeNanoseconds: h.clock.Load()}, Available: available, Failures: failures}
}
func gateWait(t *testing.T, condition func() bool, message string) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func TestBFDGateWaitsForTCPDownAckAndNewReceiveBeforeRecovery(t *testing.T) {
	h := newGateHarness(t)
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "initial healthy source did not enable")
	h.admin.mu.Lock()
	h.admin.holdDown = true
	h.admin.mu.Unlock()
	h.clock.Add(int64(20 * time.Millisecond))
	h.send(gateGrant(h.clock.Load(), 2, 1), true, 0)
	gateWait(t, func() bool { return len(h.admin.log()) == 2 }, "coalesced failure did not disable session")
	if proof := h.view.snapshot()[gatePeer]; proof.Enabled || proof.DownTransitions != 0 {
		t.Fatalf("unacknowledged source counter became a routing proof: %+v", proof)
	}
	h.clock.Add(int64(20 * time.Millisecond))
	h.send(gateGrant(h.clock.Load(), 3, 1), true, 0)
	time.Sleep(20 * time.Millisecond)
	if len(h.admin.log()) != 2 {
		t.Fatal("reenabled before ADMIN_DOWN+IDLE acknowledged TCP closure")
	}
	h.admin.mu.Lock()
	h.admin.state = api.PeerState_SESSION_STATE_IDLE
	h.admin.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if len(h.admin.log()) != 2 {
		t.Fatal("pre-close receive evidence reenables session")
	}
	_, disabled, err := h.view.readSnapshot(func() (Snapshot, error) { return Snapshot{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	h.view.acknowledge(disabled)
	h.clock.Add(int64(20 * time.Millisecond))
	h.send(gateGrant(h.clock.Load(), 4, 1), true, 0)
	gateWait(t, func() bool { p := h.view.snapshot()[gatePeer]; return p.Enabled && p.DownTransitions == 1 }, "fresh post-close evidence did not reenable")
	if got := strings.Join(h.admin.log(), ","); got != "enable,disable,enable" {
		t.Fatal(got)
	}
}

func TestBFDGateWireExpiryAndControlFreshnessAreIndependent(t *testing.T) {
	for _, mode := range []string{"wire", "control"} {
		t.Run(mode, func(t *testing.T) {
			h := newGateHarness(t)
			g := gateGrant(h.clock.Load(), 1, 0)
			if mode == "wire" {
				g.DeadlineBootNanoseconds = g.LastReceivedBootNanoseconds + int64(100*time.Millisecond)
			}
			h.send(g, true, 0)
			gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "not enabled")
			if mode == "wire" {
				h.clock.Add(int64(100 * time.Millisecond))
				h.send(g, true, 0)
			} else {
				h.clock.Add(int64(501 * time.Millisecond))
			}
			gateWait(t, func() bool { return len(h.admin.log()) == 2 && !h.view.snapshot()[gatePeer].Enabled }, "expired grant/control receipt did not close peer")
			// A replay keeps the source receipt unchanged, even when the control
			// envelope is fresh; it must not resurrect the closed session.
			h.clock.Add(int64(20 * time.Millisecond))
			h.send(g, true, 0)
			time.Sleep(20 * time.Millisecond)
			if len(h.admin.log()) != 2 {
				t.Fatal("replay renewed health")
			}
		})
	}
}

func TestBFDGateCoalescedControlFailureStillResets(t *testing.T) {
	h := newGateHarness(t)
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "not enabled")
	h.clock.Add(int64(20 * time.Millisecond))
	h.send(gateGrant(h.clock.Load(), 2, 0), true, 1)
	gateWait(t, func() bool { return len(h.admin.log()) == 2 && !h.view.snapshot()[gatePeer].Enabled }, "coalesced HTTP failure did not reset")
}

func TestBFDGateNeverTreatsEnqueuedAdminRequestAsAcknowledged(t *testing.T) {
	h := newGateHarness(t)
	h.admin.mu.Lock()
	h.admin.ignoreEnable = true
	h.admin.mu.Unlock()
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	gateWait(t, func() bool { return len(h.admin.log()) == 1 }, "enable was not requested")
	if h.view.snapshot()[gatePeer].Enabled {
		t.Fatal("nil EnablePeer return was treated as an ACK")
	}
	h.clock.Add(int64(bfdAdminTimeout + time.Millisecond))
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "acknowledgment") {
			t.Fatal(err)
		}
		h.done <- nil
	case <-time.After(time.Second):
		t.Fatal("admin ACK timeout did not terminate gate")
	}
	if h.view.snapshot()[gatePeer].Enabled {
		t.Fatal("failed gate retained enabled proof")
	}
}

func TestUngatedRunCannotIgnoreConfiguredBFD(t *testing.T) {
	err := Run(context.Background(), gateConfig(), EffectsFunc(func(context.Context, Snapshot) error { return nil }), nil)
	if err == nil {
		t.Fatal("regular Run bypassed BFD input requirement")
	}
	if err := RunWithBFD(context.Background(), gateConfig(), strings.Repeat("a", 64), 9, nil, nil, nil); err == nil {
		t.Fatal("nil BFD stream accepted")
	}
}

func TestBFDAdminAcknowledgmentPropagatesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acknowledgedAdmin(ctx, newFakeAdmin(), gatePeer, "initial"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBFDAdminReceiptDeadlineIsIndependentOfConsumerScheduling(t *testing.T) {
	second := int64(time.Second)
	for _, tc := range []struct {
		name, boot string
		received   int64
		wantError  bool
	}{
		{"timely-receipt-delayed-consumer", "boot", 2*second - 1, false},
		{"receipt-before-operation", "boot", second - 1, true},
		{"deadline-boundary", "boot", 2 * second, true},
		{"late-receipt", "boot", 2*second + 1, true},
		{"wrong-boot", "other-boot", second, true},
		{"future-receipt", "boot", 4 * second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &bfdPeerGate{address: gatePeer, enabled: true, forceReset: true, operation: "disable", operationStarted: second, operationDeadline: 2 * second, operationCancel: func() {}}
			result := bfdAdminResult{address: gatePeer, operation: "disable", received: bfd.Clock{BootID: tc.boot, BoottimeNanoseconds: tc.received}}
			err := g.completeAdmin(result, bfd.Clock{BootID: "boot", BoottimeNanoseconds: 3 * second})
			if (err != nil) != tc.wantError {
				t.Fatal("ACK validity depended on when the consumer ran", err)
			}
			if !tc.wantError && (g.enabled || g.forceReset || g.operation != "" || g.minReceive != 3*second) {
				t.Fatal("disable ACK did not fence recovery to a fresh receive", g)
			}
		})
	}
}

func TestBFDGateConsumesQueuedTimelyAckAfterConsumerPause(t *testing.T) {
	blocked, receipt, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var blockOnce, receiptOnce, releaseOnce sync.Once
	h := newGateHarnessPrepared(t, func(h *gateHarness) {
		h.admin.holdDown = true
		h.view.changed = func() {
			if h.view.snapshotWithVersion()[gatePeer].state.operation == "disable" {
				blockOnce.Do(func() { paused.Store(true); close(blocked); <-release; paused.Store(false) })
			}
		}
		h.readClock = func() (bfd.Clock, error) {
			now := h.clock.Load()
			if paused.Load() {
				receiptOnce.Do(func() { close(receipt) })
			}
			return bfd.Clock{BootID: gateBoot, BoottimeNanoseconds: now}, nil
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "initial source did not enable")
	h.clock.Add(int64(20 * time.Millisecond))
	h.send(gateGrant(h.clock.Load(), 2, 1), true, 0)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("consumer did not pause after requesting disable")
	}
	h.admin.mu.Lock()
	h.admin.state = api.PeerState_SESSION_STATE_IDLE
	h.admin.mu.Unlock()
	select {
	case <-receipt:
	case <-time.After(time.Second):
		t.Fatal("disable acknowledgment was not observed while consumer paused")
	}
	// The result has a timely monotonic receipt; give its goroutine time to
	// enqueue it while the consumer is deliberately blocked in publish.
	time.Sleep(20 * time.Millisecond)
	h.clock.Add(int64(bfdAdminTimeout + time.Millisecond))
	releaseOnce.Do(func() { close(release) })
	gateWait(t, func() bool { return h.view.snapshotWithVersion()[gatePeer].state.operation == "" }, "queued timely ACK was discarded after the consumer deadline")
	if h.view.snapshot()[gatePeer].Enabled {
		t.Fatal("delayed ACK reused pre-close receive evidence")
	}
}

func TestBFDGrantFutureReceiveNeverBecomesAuthorizedByTimer(t *testing.T) {
	h := newGateHarness(t)
	g := gateGrant(h.clock.Load()+int64(100*time.Millisecond), 1, 0)
	h.send(g, true, 0)
	select {
	case err := <-h.done:
		h.done <- nil
		if err == nil || !strings.Contains(err.Error(), "newer than its control receipt") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("future receive evidence was retained instead of rejected")
	}
	h.clock.Add(int64(200 * time.Millisecond))
	if len(h.admin.log()) != 0 || h.view.snapshot()[gatePeer].Enabled {
		t.Fatal("a future-dated grant became authorization without fresh evidence")
	}
}

func TestBFDAdminRejectsAcknowledgmentAfterContextExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := newFakeAdmin()
	admin.afterList = func(api.PeerState_AdminState) { cancel() }
	if err := acknowledgedAdmin(ctx, admin, gatePeer, "initial"); !errors.Is(err, context.Canceled) {
		t.Fatalf("context-ignoring ListPeer returned a late successful ACK: %v", err)
	}
}

func TestBFDGateRejectsQueuedAcknowledgmentPastBootDeadline(t *testing.T) {
	h := newGateHarness(t)
	h.admin.mu.Lock()
	h.admin.afterList = func(state api.PeerState_AdminState) {
		if state == api.PeerState_ADMIN_STATE_UP {
			h.clock.Add(int64(bfdAdminTimeout + time.Millisecond))
		}
	}
	h.admin.mu.Unlock()
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	select {
	case err := <-h.done:
		h.done <- nil
		if err == nil || !strings.Contains(err.Error(), "acknowledgment") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued ACK survived its CLOCK_BOOTTIME operation deadline")
	}
	if h.view.snapshot()[gatePeer].Enabled {
		t.Fatal("late acknowledgment became a routing proof")
	}
}

func TestBFDSnapshotFencesGateResetDuringRIBWalk(t *testing.T) {
	const otherPeer = "192.0.2.3"
	proof := bgpconfig.BFDAcknowledgement{ConfigDigest: strings.Repeat("b", 64), Generation: 7, ProducerEpoch: strings.Repeat("c", 32), Enabled: true}
	g := &bfdPeerGate{address: gatePeer, enabled: true, wanted: true, acknowledged: proof}
	other := &bfdPeerGate{address: otherPeer, enabled: true, wanted: true, acknowledged: proof}
	peers := map[string]*bfdPeerGate{gatePeer: g, otherPeer: other}
	view := &bfdGateView{}
	view.publish(peers)
	before := view.snapshotWithVersion()
	snapshot, _, err := view.readSnapshot(func() (Snapshot, error) {
		// These paths were read before an admin reset. A control freshness
		// failure can reset without changing the BFD source/down counter.
		old := Snapshot{Routes: []Route{{PeerAddress: gatePeer, Prefix: "203.0.113.0/24"}, {PeerAddress: otherPeer, Prefix: "198.51.100.0/24"}, {PeerAddress: "192.0.2.4", Prefix: "198.18.0.0/15"}}}
		g.wanted, g.enabled, g.operation = false, false, "disable"
		view.publish(peers)
		g.wanted, g.enabled, g.operation = true, true, ""
		view.publish(peers)
		return old, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BFD[gatePeer].Enabled || !snapshot.BFD[otherPeer].Enabled || len(snapshot.Routes) != 2 || snapshot.Routes[0].PeerAddress != otherPeer || snapshot.Routes[1].PeerAddress != "192.0.2.4" {
		t.Fatalf("old RIB inherited a new enable proof or unrelated routes were lost: %+v", snapshot)
	}
	after := view.snapshotWithVersion()
	if after[gatePeer].state.proof != before[gatePeer].state.proof || after[gatePeer].version <= before[gatePeer].version || after[otherPeer].version != before[otherPeer].version {
		t.Fatal("ABA reset was lost or unrelated peer version changed")
	}
	// Packet/sequence refreshes do not represent an admin transition and
	// cannot continually invalidate an otherwise slow, consistent RIB walk.
	version := after[gatePeer].version
	for i := 0; i < 10; i++ {
		g.grant.Sequence++
		g.grant.LastReceivedBootNanoseconds++
		view.publish(peers)
	}
	if view.snapshotWithVersion()[gatePeer].version != version {
		t.Fatal("packet refresh advanced the RIB reset fence")
	}
	fresh, _, err := view.readSnapshot(func() (Snapshot, error) {
		return Snapshot{Routes: []Route{{PeerAddress: gatePeer, Prefix: "203.0.113.0/24"}}}, nil
	})
	if err != nil || !fresh.BFD[gatePeer].Enabled || len(fresh.Routes) != 1 {
		t.Fatalf("stable new RIB could not recover: %+v, %v", fresh, err)
	}
}

func TestBFDControlOnlyRecoveryWaitsForMatchingWithdrawalAcknowledgment(t *testing.T) {
	h := newGateHarness(t)
	h.send(gateGrant(h.clock.Load(), 1, 0), true, 0)
	gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "initial peer was not enabled")
	// An old successful RIB read can remain in a slow Effects request while
	// the gate closes the peer. Its eventual ACK must never release a reset.
	oldSnapshot, oldPublication, err := h.view.readSnapshot(func() (Snapshot, error) {
		return Snapshot{Routes: []Route{{PeerAddress: gatePeer, Prefix: "203.0.113.0/24"}}}, nil
	})
	if err != nil || !oldSnapshot.BFD[gatePeer].Enabled {
		t.Fatal("missing old enabled proof", err)
	}
	var previousWithdrawal map[string]uint64
	for reset := uint64(1); reset <= 2; reset++ {
		h.clock.Add(int64(20 * time.Millisecond))
		h.send(gateGrant(h.clock.Load(), reset*2, 0), true, reset)
		gateWait(t, func() bool {
			entry := h.view.snapshotWithVersion()[gatePeer]
			return entry.state.needsWithdrawal && !entry.state.enabled && entry.state.operation == ""
		}, "control failure did not close TCP before waiting for Effects")
		_, withdrawal, err := h.view.readSnapshot(func() (Snapshot, error) { return Snapshot{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		h.clock.Add(int64(20 * time.Millisecond))
		h.send(gateGrant(h.clock.Load(), reset*2+1, 0), true, reset)
		h.view.acknowledge(oldPublication)
		h.view.acknowledge(previousWithdrawal)
		time.Sleep(20 * time.Millisecond)
		if h.view.snapshot()[gatePeer].Enabled || len(h.admin.log()) != int(reset*2) {
			t.Fatal("old in-flight/previous-reset ACK released a newer withdrawal fence")
		}
		// The writer has now acknowledged the exact disabled publication.
		// New source packets were received after close; recovery may proceed.
		h.view.acknowledge(withdrawal)
		gateWait(t, func() bool { return h.view.snapshot()[gatePeer].Enabled }, "matching withdrawal ACK did not permit recovery")
		if h.view.snapshot()[gatePeer].DownTransitions != 0 {
			t.Fatal("test unexpectedly relied on a BFD Down counter change")
		}
		previousWithdrawal = withdrawal
	}
}

func TestBFDReferenceChangeInvalidatesPersistedDynamicIntent(t *testing.T) {
	cfg := moduleConfig(t)
	cfg.Runtime.Peers = gateConfig().Runtime.Peers
	cfg.PeerBFD = gateConfig().PeerBFD
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	applied, _, err := readApplied(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	applied.Paths = append(applied.Paths, bgpdaemon.AppliedPath{Source: "MobilityPool/lab/node/a", Prefix: "10.99.0.8/32", UUID: "1234"})
	if err := writeApplied(cfg.StatePath, applied); err != nil {
		t.Fatal(err)
	}
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	restored, _, err := readApplied(cfg.StatePath)
	if err != nil || len(bgpdaemon.NonStaticPaths(restored.Paths)) != 1 {
		t.Fatal("same BFD intent lost its journal", err)
	}
	cfg.PeerBFD[gatePeer] = bgpconfig.BFDReference{Service: "replacement-health", Address: gatePeer}
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	restored, _, err = readApplied(cfg.StatePath)
	if err != nil || len(bgpdaemon.NonStaticPaths(restored.Paths)) != 0 {
		t.Fatal("new BFD source inherited old dynamic intent", err)
	}
}
