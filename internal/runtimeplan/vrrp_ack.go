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
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// One pending effect per bound VRRP producer. Accept records an observation;
// only the serialized coordinator can acknowledge its kernel effects. In
// particular, no protocol callback acquires op or changes the module set.
type vrrpEffect struct {
	update      serviceconfig.Update
	sourceEpoch uint64
	done        chan struct{}
	err         error
}

func sameVRRPUpdate(a, b serviceconfig.Update) bool {
	return a.SupervisorInstance == b.SupervisorInstance && a.Name == b.Name && a.ConfigDigest == b.ConfigDigest && a.Generation == b.Generation && a.ProducerEpoch == b.ProducerEpoch && a.Sequence == b.Sequence && bytes.Equal(a.Snapshot, b.Snapshot)
}

// Caller holds mu. Wait releases it so the actor can apply and acknowledge.
func (r *Runtime) waitVRRPEffectLocked(ctx context.Context, u serviceconfig.Update) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if r.vrrpEffects == nil {
		r.vrrpEffects = make(map[string]*vrrpEffect)
	}
	effect := r.vrrpEffects[u.Name]
	if effect == nil || effect.sourceEpoch != r.sourceEpoch || !sameVRRPUpdate(effect.update, u) {
		if effect != nil {
			r.finishVRRPEffectLocked(u.Name, ErrStale)
		}
		u.Snapshot = append(json.RawMessage(nil), u.Snapshot...)
		effect = &vrrpEffect{update: u, sourceEpoch: r.sourceEpoch, done: make(chan struct{})}
		r.vrrpEffects[u.Name] = effect
	}
	r.wakeVRRPEffectsLocked()
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

func (r *Runtime) finishVRRPEffectLocked(name string, err error) {
	if effect := r.vrrpEffects[name]; effect != nil {
		effect.err = err
		delete(r.vrrpEffects, name)
		close(effect.done)
	}
}

// Fence an accepted but unapplied observation before it can enter a kernel
// projection after its registered worker has gone away or been replaced.
func (r *Runtime) fenceVRRPEffectsLocked() {
	for name, effect := range r.vrrpEffects {
		module, ok := r.guardStatus[name]
		if ok && effect.sourceEpoch == r.sourceEpoch && module.Role == "vrrp" && module.Generation == effect.update.Generation && module.ConfigDigest == effect.update.ConfigDigest && r.validateVRRPAdmissionLocked(effect.update) == nil {
			continue
		}
		if current, exists := r.records[name]; exists && sameVRRPUpdate(current, effect.update) {
			delete(r.records, name)
			delete(r.leases, name)
			delete(r.guardGrants, name)
		}
		r.finishVRRPEffectLocked(name, ErrStale)
	}
}

func (r *Runtime) finishVRRPEffectsLocked(d network.Desired, applyErr error) {
	for name, effect := range r.vrrpEffects {
		err := applyErr
		current, ok := r.records[name]
		binding, bound := r.bindings[name]
		module, registered := r.guardStatus[name]
		if !ok || !bound || !registered || module.Role != "vrrp" || module.Generation != effect.update.Generation || module.ConfigDigest != effect.update.ConfigDigest || binding.digest != effect.update.ConfigDigest || effect.sourceEpoch != r.sourceEpoch || !sameVRRPUpdate(current, effect.update) {
			err = ErrStale
		}
		if err == nil {
			err = vrrpEffectApplied(effect.update, d)
		}
		r.finishVRRPEffectLocked(name, err)
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

func (r *Runtime) pendingVRRPWithdrawalLocked(name string) bool {
	effect := r.vrrpEffects[name]
	if effect == nil || effect.sourceEpoch != r.sourceEpoch {
		return false
	}
	current, ok := r.records[name]
	if !ok || !sameVRRPUpdate(current, effect.update) {
		return false
	}
	var s vrrp.Snapshot
	return json.Unmarshal(current.Snapshot, &s) == nil && !s.Active
}

// wait runs with op held during worker startup/stop. It must drain effects too:
// initial Ready and final worker exit both depend on a synchronous VRRP ACK.
// This path may stop workers, but never starts newly eligible dependents. All
// other retiring workers must have exited before network withdrawal; a VRRP
// producer waiting for its own inactive ACK is the sole exception.
func (r *Runtime) drainVRRPEffects(ctx context.Context, status supervisor.Status, removed map[string]bool) error {
	r.mu.Lock()
	if len(r.vrrpEffects) == 0 {
		r.mu.Unlock()
		return nil
	}
	r.installGuardStatusLocked(status, nil)
	r.fenceVRRPEffectsLocked()
	p, err := r.projectProgramLocked(r.config, r.effectiveRecordsLocked(time.Now()), time.Now())
	if err != nil {
		r.finishVRRPEffectsLocked(network.Desired{}, err)
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
		if (removed[module.Name] || installed[module.Name] != module.ConfigDigest) && !r.pendingVRRPWithdrawalLocked(module.Name) {
			r.mu.Unlock()
			return nil
		}
	}
	p, err = r.applyVRRPProjectionLocked(ctx, p)
	if err == nil {
		r.resourceStates, r.liveRA, r.routeIssues, r.bgpApplied = p.resources, p.liveRA, p.routeIssues, p.bgpStatus
	}
	r.finishVRRPEffectsLocked(p.network, err)
	r.mu.Unlock()
	return err
}

// Separate coalescing signals prevent Monitor from consuming a wake intended
// for an ApplyConfig/stop waiter that currently holds op.
func (r *Runtime) wakeVRRPEffectsLocked() {
	select {
	case r.reconcileWake <- struct{}{}:
	default:
	}
	select {
	case r.waitWake <- struct{}{}:
	default:
	}
}
