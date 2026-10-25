package runtimeplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// One pending application per bound BGP/VRRP producer. Observe records input;
// only the serialized coordinator can acknowledge its kernel effects. In
// particular, no protocol callback acquires op or changes the module set.
type pendingApplication struct {
	update      serviceconfig.Update
	sourceEpoch uint64
	done        chan struct{}
	err         error
}

func sameUpdate(a, b serviceconfig.Update) bool {
	return a.SupervisorInstance == b.SupervisorInstance && a.Name == b.Name && a.Role == b.Role && a.ConfigDigest == b.ConfigDigest && a.Generation == b.Generation && a.ProducerEpoch == b.ProducerEpoch && a.Sequence == b.Sequence && bytes.Equal(a.Snapshot, b.Snapshot)
}

// Caller holds mu. Wait releases it so the actor can apply and acknowledge.
func (r *Runtime) waitApplicationLocked(ctx context.Context, u serviceconfig.Update) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	switch u.Role {
	case "vrrp":
		var s vrrp.Snapshot
		if err := json.Unmarshal(u.Snapshot, &s); err != nil {
			return err
		}
		if s.Active && !vrrp.CurrentOwnership(s, clock) {
			return ErrStale
		}
	case "bgp":
		provider, err := bgpProviderFor(u)
		if err != nil {
			return err
		}
		if provider.snapshot.Active && !bgpconfig.CurrentObservation(provider.snapshot, clock) {
			return ErrStale
		}
	}
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
		r.applications[u.Name] = effect
	}
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
		return err
	}
	return effect.err
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
		if valid && effect.update.Role == "vrrp" {
			valid = r.validateVRRPAdmissionLocked(effect.update) == nil
		}
		if valid {
			continue
		}
		if current, exists := r.records[name]; exists && sameUpdate(current, effect.update) {
			delete(r.records, name)
			delete(r.leases, name)
			delete(r.guardGrants, name)
			r.observationVersion++
		}
		r.finishApplicationLocked(name, ErrStale)
	}
}

func (r *Runtime) finishApplicationsLocked(p projection, applyErr error) {
	for name, effect := range r.applications {
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
				err = vrrpEffectApplied(effect.update, p.network)
			case "bgp":
				err = bgpApplicationApplied(effect.update, p)
			}
		}
		r.finishApplicationLocked(name, err)
	}
}

func vrrpEffectApplied(u serviceconfig.Update, d network.Desired) error {
	var s vrrp.Snapshot
	if err := json.Unmarshal(u.Snapshot, &s); err != nil {
		return err
	}
	if s.Active {
		clock, err := boottime.Read()
		if err != nil || !vrrp.CurrentOwnership(s, clock) {
			return errors.Join(ErrStale, err)
		}
	}
	linkPresent := false
	for _, link := range d.Links {
		if link.Name == s.VIPInterface {
			linkPresent = true
			if s.Active && (link.Kind != "macvlan" || link.Parent != s.Interface || link.HardwareAddr != s.VirtualMAC) {
				return errors.New("VRRP VMAC effect does not match publication")
			}
		}
	}
	if linkPresent != s.Active {
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
				if s.Active && (opts.BootLifetime.BootID != s.BootID || opts.BootLifetime.ValidNanos != s.ExpiresNanos || opts.BootLifetime.PreferredNanos != s.ExpiresNanos) {
					return errors.New("VRRP VIP effect changed original authority")
				}
			}
		}
		if found != s.Active {
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
	var s struct {
		Active bool `json:"active"`
	}
	return json.Unmarshal(current.Snapshot, &s) == nil && !s.Active
}

// wait runs with op held during worker startup/stop. It must drain effects too:
// BGP/VRRP initial Ready and final withdrawal can depend on an application ACK.
// This path may stop workers, but never starts newly eligible dependents. All
// other retiring workers must have exited before network withdrawal; a producer
// waiting for its own inactive application ACK is the exception.
func (r *Runtime) reconcileWhileWaiting(ctx context.Context, status supervisor.Status, removed map[string]bool) error {
	r.mu.Lock()
	if !r.configured {
		r.mu.Unlock()
		return nil
	}
	r.installGuardStatusLocked(status, nil)
	r.fenceApplicationsLocked()
	p, err := r.projectProgramLocked(r.config, r.effectiveRecordsLocked(time.Now()), time.Now())
	if err != nil {
		r.finishApplicationsLocked(projection{}, err)
		r.mu.Unlock()
		return err
	}
	desired := make(map[string]string, len(p.instances))
	for _, instance := range p.instances {
		_, digest, encodeErr := encodeInstance(instance)
		if encodeErr != nil {
			r.mu.Unlock()
			return encodeErr
		}
		desired[instance.Name] = digest
	}
	keep := make([]supervisor.DynamicModule, 0, len(r.installed))
	for _, module := range r.installed {
		if desired[module.Name] == module.ConfigDigest && !removed[module.Name] {
			keep = append(keep, module)
		}
	}
	if len(keep) != len(r.installed) {
		r.mu.Unlock()
		if err := r.manager.Reconcile(ctx, keep); err != nil {
			return err
		}
		r.installed = keep
		return nil // obtain a new supervisor observation before applying effects
	}
	installed := make(map[string]string, len(keep))
	for _, module := range keep {
		installed[module.Name] = module.ConfigDigest
	}
	for _, module := range status.Modules {
		if !strings.HasPrefix(module.Name, "service.") {
			continue
		}
		if (removed[module.Name] || installed[module.Name] != module.ConfigDigest) && !r.pendingWithdrawalLocked(module.Name) {
			r.mu.Unlock()
			return nil
		}
	}
	_, err = r.applyProjectionLocked(ctx, p)
	r.mu.Unlock()
	return err
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
	p, err := r.applyVRRPProjectionLocked(ctx, p)
	if err == nil {
		r.resourceStates, r.liveRA, r.routeIssues, r.bgpApplied = p.resources, p.liveRA, p.routeIssues, p.bgpStatus
		r.applied.observationVersion = p.observationVersion
		r.applied.sourceEpoch = r.sourceEpoch
		if p.observationVersion != r.observationVersion {
			r.wakeCoordinatorLocked()
			return p, nil
		}
		for _, record := range r.records {
			if record.Role == "bgp" {
				r.acceptBFDRecipientLocked(record)
			}
		}
	} else {
		r.applied.hash = ""
	}
	r.finishApplicationsLocked(p, err)
	return p, err
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
