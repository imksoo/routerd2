package runtimeplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// One pending application per bound producer. Observe records input;
// only the serialized coordinator can acknowledge its kernel effects. In
// particular, no protocol callback acquires op or changes the module set.
type pendingApplication struct {
	update      serviceconfig.Update
	sourceEpoch uint64
	done        chan struct{}
	err         error
	receipt     serviceconfig.ApplicationReceipt
	withdrawRA  map[string][]string
}

func sameUpdate(a, b serviceconfig.Update) bool {
	return a.SupervisorInstance == b.SupervisorInstance && a.Name == b.Name && a.Role == b.Role && a.ConfigDigest == b.ConfigDigest && a.Generation == b.Generation && a.ProducerEpoch == b.ProducerEpoch && a.Sequence == b.Sequence && bytes.Equal(a.Snapshot, b.Snapshot)
}

// Caller holds mu. Wait releases it so the actor can apply and acknowledge.
func (r *Runtime) waitApplicationLocked(ctx context.Context, u serviceconfig.Update) (serviceconfig.ApplicationReceipt, error) {
	if err := ctx.Err(); err != nil {
		return serviceconfig.ApplicationReceipt{}, err
	}
	clock, err := boottime.Read()
	if err != nil {
		return serviceconfig.ApplicationReceipt{}, err
	}
	switch u.Role {
	case "vrrp":
		var s vrrp.Snapshot
		if err := json.Unmarshal(u.Snapshot, &s); err != nil {
			return serviceconfig.ApplicationReceipt{}, err
		}
		if s.State == vrrp.StateActive && !vrrp.CurrentElection(s, clock) {
			return serviceconfig.ApplicationReceipt{}, ErrStale
		}
	case "bgp":
		provider, err := bgpProviderFor(u)
		if err != nil {
			return serviceconfig.ApplicationReceipt{}, err
		}
		if provider.snapshot.Active && !bgpconfig.CurrentObservation(provider.snapshot, clock) {
			return serviceconfig.ApplicationReceipt{}, ErrStale
		}
	}
	return r.waitEffectLocked(ctx, r.queueApplicationLocked(u))
}

func (r *Runtime) waitEffectLocked(ctx context.Context, effect *pendingApplication) (serviceconfig.ApplicationReceipt, error) {
	r.wakeCoordinatorLocked()
	r.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-effect.done:
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		// Other transport retries can still wait on this same observation. Dropping
		// the caller does not revoke or renew a fact or manufacture a kernel ACK.
		return serviceconfig.ApplicationReceipt{}, err
	}
	return effect.receipt, effect.err
}

func (r *Runtime) queueApplicationLocked(u serviceconfig.Update) *pendingApplication {
	if r.applications == nil {
		r.applications = make(map[string]*pendingApplication)
	}
	effect := r.applications[u.Name]
	if effect == nil || effect.sourceEpoch != r.sourceEpoch || !sameUpdate(effect.update, u) {
		if effect != nil {
			r.finishApplicationLocked(u.Name, ErrStale)
		}
		u.Snapshot = append(json.RawMessage(nil), u.Snapshot...)
		effect = &pendingApplication{update: u, sourceEpoch: r.sourceEpoch, done: make(chan struct{})}
		if u.Role == "dhcp6" {
			effect.withdrawRA = r.dhcp6WithdrawalsLocked(u.Name)
		}
		r.applications[u.Name] = effect
	}
	return effect
}

func (r *Runtime) finishApplicationLocked(name string, err error) {
	if effect := r.applications[name]; effect != nil {
		effect.err = err
		delete(r.applications, name)
		close(effect.done)
	}
}

// Fence an accepted but unapplied observation before it can enter a kernel
// projection after its registered worker has gone away or been replaced.
func (r *Runtime) fenceApplicationsLocked() {
	for name, effect := range r.applications {
		module, ok := r.guardStatus[name]
		valid := ok && effect.sourceEpoch == r.sourceEpoch && module.Role == effect.update.Role && module.Generation == effect.update.Generation && module.ConfigDigest == effect.update.ConfigDigest
		if valid {
			continue
		}
		if current, exists := r.records[name]; exists && sameUpdate(current, effect.update) {
			delete(r.records, name)
			delete(r.guardGrants, name)
			r.observationVersion++
		}
		r.finishApplicationLocked(name, ErrStale)
	}
}

func (r *Runtime) finishApplicationsLocked(p projection, applications map[string]*pendingApplication, applyErr error) {
	for name, effect := range applications {
		if r.applications[name] != effect {
			continue
		}
		err := applyErr
		current, ok := r.records[name]
		binding, bound := r.bindings[name]
		module, registered := r.guardStatus[name]
		if !ok || !bound || !registered || module.Role != effect.update.Role || module.Generation != effect.update.Generation || module.ConfigDigest != effect.update.ConfigDigest || binding.digest != effect.update.ConfigDigest || effect.sourceEpoch != r.sourceEpoch || !sameUpdate(current, effect.update) {
			err = ErrStale
		}
		if err == nil {
			switch effect.update.Role {
			case "vrrp":
				_, active := p.vrrpOwners[name]
				err = vrrpEffectApplied(effect.update, p.network, active)
				if err == nil {
					effect.receipt.VRRP, err = r.vrrpDecisionLocked(name, active)
				}
			case "bgp":
				err = bgpApplicationApplied(effect.update, p)
			case "dhcp6":
				err = r.dhcp6WithdrawalAppliedLocked(effect.withdrawRA)
			}
		}
		effect.receipt.Applied = err == nil
		r.finishApplicationLocked(name, err)
	}
}

func vrrpEffectApplied(u serviceconfig.Update, d network.Desired, active bool) error {
	var s vrrp.Snapshot
	if err := json.Unmarshal(u.Snapshot, &s); err != nil {
		return err
	}
	if s.State == vrrp.StateActive {
		clock, err := boottime.Read()
		if err != nil || !vrrp.CurrentElection(s, clock) {
			return errors.Join(ErrStale, err)
		}
	}
	linkPresent := false
	for _, link := range d.Links {
		if link.Name == s.VIPInterface {
			linkPresent = true
			if active && (link.Kind != "macvlan" || link.Parent != s.Interface || link.HardwareAddr != s.VirtualMAC || link.Authority.BootID != s.BootID || link.Authority.Nanos != s.ExpiresNanos) {
				return errors.New("VRRP VMAC effect does not match publication")
			}
		}
	}
	if linkPresent != active {
		return errors.New("VRRP ownership is not in the applied resource projection")
	}
	for _, address := range s.Addresses {
		found := false
		for _, iface := range d.Interfaces {
			if iface.Name != s.VIPInterface {
				continue
			}
			for _, actual := range iface.Addresses {
				if actual != address {
					continue
				}
				found = true
				opts := iface.AddressOptions[address]
				if active && (opts.BootLifetime.BootID != s.BootID || opts.BootLifetime.ValidNanos != s.ExpiresNanos || opts.BootLifetime.PreferredNanos != s.ExpiresNanos) {
					return errors.New("VRRP VIP effect changed original authority")
				}
			}
		}
		if found != active {
			return errors.New("VRRP VIP effect does not match publication")
		}
	}
	return nil
}

func (r *Runtime) pendingWithdrawalLocked(name string) bool {
	effect := r.applications[name]
	if effect == nil || effect.sourceEpoch != r.sourceEpoch {
		return false
	}
	current, ok := r.records[name]
	if !ok || !sameUpdate(current, effect.update) {
		return false
	}
	if current.Role == "vrrp" {
		var s vrrp.Snapshot
		return json.Unmarshal(current.Snapshot, &s) == nil && s.State != vrrp.StateActive
	}
	if current.Role == "dhcp6" {
		var s dhcp6config.Snapshot
		return json.Unmarshal(current.Snapshot, &s) == nil && s.State == "stopped"
	}
	var s struct {
		Active bool `json:"active"`
	}
	return json.Unmarshal(current.Snapshot, &s) == nil && !s.Active
}

// One owner iteration handles both ordinary reconciliation and transaction
// waits. Supervisor status is the process authority; no second installed set
// predicts whether a worker has actually stopped.
func (r *Runtime) reconcileStep(ctx context.Context, ceiling map[string]supervisor.DynamicModule) (bool, error) {
	status, err := r.manager.Status(ctx)
	r.mu.Lock()
	r.installGuardStatusLocked(status, err)
	if err != nil {
		r.mu.Unlock()
		return false, err
	}
	r.fenceApplicationsLocked()
	var p projection
	if r.configured {
		p, err = r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
	}
	if err != nil {
		r.mu.Unlock()
		return false, err
	}
	drainingDHCP := false
	if status.Stopping {
		// Keep reconciliation within the current process set during shutdown.
		ceiling = make(map[string]supervisor.DynamicModule)
		for _, module := range status.Modules {
			ceiling[module.Name] = supervisor.DynamicModule{Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest}
		}
	} else if ceiling != nil {
		for _, module := range status.Modules {
			actual := supervisor.DynamicModule{Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest}
			if module.Role != "dhcp6" || ceiling[module.Name] == actual {
				continue
			}
			// Retire DHCPv6 first while the old WAN and its other consumers
			// still exist. The stopped observation retires its generated RA.
			drainingDHCP = true
			ceiling = maps.Clone(ceiling)
			for _, peer := range status.Modules {
				if peer.Role != "dhcp6" {
					ceiling[peer.Name] = supervisor.DynamicModule{Name: peer.Name, Role: peer.Role, ConfigDigest: peer.ConfigDigest}
				}
			}
			break
		}
	}
	set := selectInstances(p.instances, ceiling)
	changed := len(set.bindings) != len(r.bindings)
	for name, b := range set.bindings {
		if old, ok := r.bindings[name]; !ok || old.digest != b.digest {
			changed = true
		}
	}
	r.mu.Unlock()
	var sources map[string]bfdBinding
	var recipients map[string]bfdRecipient
	if changed {
		sources, recipients, err = compileBFDCache(set.bindings)
		if err != nil {
			return false, err
		}
		if err := r.prepare(set); err != nil {
			return false, err
		}
		// Filesystem work can outlast the observation used to choose this set.
		status, err = r.manager.Status(ctx)
		if err != nil {
			return false, err
		}
	}
	keep := make([]supervisor.DynamicModule, 0, len(status.Modules))
	retire, blocked, matching := false, false, true
	ready := true
	var moduleFailure error
	r.mu.Lock()
	if changed {
		r.installGuardStatusLocked(status, nil)
		r.fenceApplicationsLocked()
	}
	for _, module := range status.Modules {
		if !strings.HasPrefix(module.Name, "service.") {
			continue
		}
		actual := supervisor.DynamicModule{Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest}
		if expected, ok := set.bindings[module.Name]; ok && expected.digest == module.ConfigDigest && expected.instance.Role == module.Role {
			keep = append(keep, actual)
			ready = ready && module.Ready
			if module.State == "failed" {
				moduleFailure = fmt.Errorf("%s failed: %s", module.Name, module.LastError)
			}
			continue
		}
		matching = false
		retire = retire || module.State != "stopping"
		blocked = blocked || !r.pendingWithdrawalLocked(module.Name)
	}
	if !matching || len(keep) != len(set.modules) || !ready {
		r.phase = "applying"
	}
	r.mu.Unlock()
	if retire {
		if err := r.manager.Reconcile(ctx, keep); err != nil {
			return false, err
		}
		return false, nil // consume a new supervisor observation before effects
	}
	if blocked {
		return false, nil
	}
	r.mu.Lock()
	if !r.configured {
		r.mu.Unlock()
		return matching, nil
	}
	latest, err := r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
	if err != nil {
		r.mu.Unlock()
		return false, err
	}
	if !sameInstances(latest.instances, p.instances) {
		r.wakeCoordinatorLocked()
		r.mu.Unlock()
		return false, nil
	}
	p, err = r.applyProjectionLocked(ctx, latest)
	if err != nil {
		r.mu.Unlock()
		return false, err
	}
	if p.observationVersion != r.observationVersion {
		// New observations can arrive during kernel I/O. Do not start workers
		// against a projection that no longer describes their prerequisites.
		current, projectErr := r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
		if projectErr != nil {
			r.mu.Unlock()
			return false, projectErr
		}
		if !reflect.DeepEqual(current.network, p.network) || !sameInstances(current.instances, p.instances) {
			r.wakeCoordinatorLocked()
			r.mu.Unlock()
			return false, nil
		}
	}
	if changed {
		r.bindings = set.bindings
		r.installBFDCacheLocked(sources, recipients)
		for name, input := range r.aftrInputs {
			if b, ok := r.bindings[name]; !ok || b.digest != input.ConfigDigest {
				delete(r.aftrInputs, name)
			}
		}
		for name := range r.records {
			if _, ok := r.bindings[name]; !ok {
				delete(r.records, name)
				delete(r.dhcpServers, name)
				delete(r.guardGrants, name)
			}
		}
	}
	r.mu.Unlock()
	// Cutover waits for old identities to exit. Readiness of a retained worker
	// can depend on the new configuration, so it cannot gate accepting it.
	if ceiling != nil {
		// A child can be reaped during preparation, before the second status
		// read. Matching the intermediate DHCP drain still retains its WAN;
		// only a fresh pass against the requested ceiling can finish cutover.
		return matching && !drainingDHCP, nil
	}
	if !matching || len(keep) != len(set.modules) {
		if err := r.manager.Reconcile(ctx, set.modules); err != nil {
			return false, err
		}
		return false, nil
	}
	return ready, moduleFailure
}

// Separate coalescing signals prevent Monitor from consuming a wake intended
// for an ApplyConfig/stop waiter that currently holds op.
func (r *Runtime) wakeCoordinatorLocked() {
	select {
	case r.reconcileWake <- struct{}{}:
	default:
	}
	select {
	case r.waitWake <- struct{}{}:
	default:
	}
}

// The operation owner holds op and mu. Protocol callbacks only record input;
// network effects, published views and acknowledgements complete here.
func (r *Runtime) applyProjectionLocked(ctx context.Context, p projection) (projection, error) {
	// Only these requests belong to this kernel operation. Later observations
	// need their own application, but cannot delay ACKs of completed effects.
	applications := maps.Clone(r.applications)
	var recipients []serviceconfig.Update
	for _, record := range r.records {
		if record.Role == "bgp" {
			recipients = append(recipients, record)
		}
	}
	if p.activationUntil != 0 {
		clock, err := boottime.Read()
		if err != nil || uint64(clock.BoottimeNanoseconds) >= p.activationUntil {
			err = errors.Join(ErrStale, err)
			r.finishApplicationsLocked(p, applications, err)
			return p, err
		}
	}
	// Admission is consumed at the start of this kernel operation. Subsequent
	// readiness loss does not revoke a successfully applied election.
	err := r.applyNetworkLocked(ctx, p.network)
	if err == nil {
		r.resourceStates, r.liveRA, r.routeIssues, r.bgpApplied = p.resources, p.liveRA, p.routeIssues, p.bgpStatus
		r.vrrpOwners = p.vrrpOwners
		r.applied.observationVersion = p.observationVersion
		r.applied.sourceEpoch = r.sourceEpoch
		until := uint64(math.MaxUint64)
		for _, link := range p.network.Links {
			if link.Authority.BootID != "" && link.Authority.Nanos != 0 {
				until = min(until, link.Authority.Nanos)
			}
		}
		for _, route := range p.network.Routes {
			if route.ValidUntil.BootID != "" && route.ValidUntil.Nanos != 0 {
				until = min(until, route.ValidUntil.Nanos)
			}
			if route.BootID != "" {
				until = min(until, route.ExpiresNanos)
			}
		}
		for _, iface := range p.network.Interfaces {
			for _, address := range iface.Addresses {
				if lifetime := iface.AddressOptions[address].BootLifetime; lifetime.BootID != "" {
					until = min(until, lifetime.ValidNanos)
				}
			}
		}
		r.applied.untilNanos = 0
		if until != math.MaxUint64 {
			r.applied.untilNanos = until
		}
		if p.observationVersion != r.observationVersion {
			r.wakeCoordinatorLocked()
		}
		for _, record := range recipients {
			if sameUpdate(r.records[record.Name], record) {
				r.acceptBFDRecipientLocked(record)
			}
		}
	} else {
		r.applied.hash = ""
	}
	r.finishApplicationsLocked(p, applications, err)
	return p, err
}

// Capture the advertised prefixes before accepting the stopped observation.
// The coordinator may run before the HTTP handler starts waiting for its ACK.
func (r *Runtime) dhcp6WithdrawalsLocked(name string) map[string][]string {
	required := make(map[string][]string)
	for _, service := range r.config.plan.Native.Services {
		if service.ModuleName() != name || service.DHCP6 == nil {
			continue
		}
		for _, lan := range service.DHCP6.Delegations {
			module := name + "-" + lan.Name + "-ra"
			for _, prefix := range r.liveRA[module].Prefixes {
				required[module] = append(required[module], prefix.Prefix)
			}
		}
	}
	return required
}

func (r *Runtime) dhcp6WithdrawalAppliedLocked(required map[string][]string) error {
	for module, prefixes := range required {
		var snapshot ra.Snapshot
		record, ok := r.records[module]
		if !ok || json.Unmarshal(record.Snapshot, &snapshot) != nil || !snapshot.Withdrawing || snapshot.Sent == 0 {
			return fmt.Errorf("%s has not acknowledged its final RA withdrawal", module)
		}
		for _, prefix := range prefixes {
			if !slices.Contains(snapshot.WithdrawingPrefixes, prefix) {
				return fmt.Errorf("%s has not withdrawn prefix %s", module, prefix)
			}
		}
	}
	return nil
}

func bgpApplicationApplied(u serviceconfig.Update, p projection) error {
	provider, err := bgpProviderFor(u)
	if err != nil {
		return err
	}
	status, present := p.bgpStatus[u.Name]
	if !provider.snapshot.Active {
		if present {
			return errors.New("BGP withdrawal remains in the applied projection")
		}
		return nil
	}
	clock, err := boottime.Read()
	if err != nil || !bgpconfig.CurrentObservation(provider.snapshot, clock) {
		return errors.Join(ErrStale, err)
	}
	if !present || status.identity != provider.projection {
		return errors.New("BGP observation is not in the applied FIB projection")
	}
	return nil
}
