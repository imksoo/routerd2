package runtimeplan

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// guardGrant retains source age and its original deadline. Re-projecting,
// repeating a sequence or stepping wall time cannot renew protocol authority.
type guardGrant struct {
	revision, generation, sequence uint64
	epoch, bootID                  string
	value                          bool
	deadline                       time.Time
	untilNanos                     uint64
	observedNanos                  uint64
	bgp                            *bgpProvider
	dhcpLease                      dhcp4.LeaseClock
	health                         *health.Snapshot
}

func dynamicProgram(source config.Document) bool {
	for _, r := range source.Resources {
		if r.When != nil || r.ReadyWhen != nil || r.Service != nil && r.Service.VRRP != nil && r.Service.VRRP.GracefulActivation != nil {
			return true
		}
		if r.Address != nil && r.Address.Options != nil && !r.Address.Options.ValidUntil.IsZero() || r.Route != nil && !r.Route.ValidUntil.IsZero() {
			return true
		}
	}
	return false
}

func (r *Runtime) installGuardStatusLocked(status supervisor.Status, err error) {
	clock, clockErr := bfd.ReadClock()
	r.guardStatus = make(map[string]supervisor.ModuleStatus)
	if err == nil && clockErr == nil && !status.Stopping {
		for _, module := range status.Modules {
			r.guardStatus[module.Name] = module
		}
	}
	// An unavailable supervisor produces an empty (unknown) snapshot, so its
	// failure withdraws dependent work rather than preserving the last Up.
	r.guardUntil = time.Now().Add(time.Second)
	r.guardBootID = clock.BootID
	r.guardUntilNanos = uint64(clock.BoottimeNanoseconds) + uint64(time.Second)
	if len(r.config.plan.Native.DerivedEvents) > 0 {
		_, _ = r.conditionFactsLocked(r.config, time.Now())
	} // clock failure remains unknown
}

func (r *Runtime) recordGuardGrantLocked(u serviceconfig.Update, now time.Time) {
	if u.Role == "dhcp4" {
		var s dhcp4.Snapshot
		if json.Unmarshal(u.Snapshot, &s) != nil {
			delete(r.guardGrants, u.Name)
			return
		}
		observed, until := s.ObservedNanos, s.ObservedNanos+uint64(5*time.Second)
		if s.Active {
			observed, until = s.LeaseClock.AcquiredNanos, s.LeaseClock.ExpiresNanos
		}
		if old, ok := r.guardGrants[u.Name]; ok && old.revision == r.config.revision && old.generation == u.Generation && old.epoch == u.ProducerEpoch && old.bootID == s.BootID && old.value == s.Active && (s.Active && old.dhcpLease == s.LeaseClock || !s.Active && old.observedNanos == s.ObservedNanos) {
			until = min(until, old.untilNanos)
		}
		if r.guardGrants == nil {
			r.guardGrants = map[string]guardGrant{}
		}
		r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: s.BootID, value: s.Active, observedNanos: observed, untilNanos: until, dhcpLease: s.LeaseClock}
		return
	}
	if u.Role == "bgp" {
		provider, err := bgpProviderFor(u)
		if err != nil {
			delete(r.guardGrants, u.Name)
			return
		}
		if r.guardGrants == nil {
			r.guardGrants = map[string]guardGrant{}
		}
		s := provider.snapshot
		r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: s.BootID, value: s.Active, untilNanos: s.ExpiresNanos, observedNanos: s.ObservedNanos, deadline: s.ValidUntil, bgp: &provider}
		return
	}
	if u.Role == "health" {
		var snapshot health.Snapshot
		if json.Unmarshal(u.Snapshot, &snapshot) != nil {
			return
		}
		if r.guardGrants == nil {
			r.guardGrants = make(map[string]guardGrant)
		}
		r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: snapshot.BootID, value: snapshot.Active, untilNanos: snapshot.ExpiresNanos, observedNanos: snapshot.ObservedNanos, health: &snapshot}
		return
	}
	if u.Role != "dhcp6pd" {
		return
	}
	var snapshot struct {
		Active    bool      `json:"active"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if json.Unmarshal(u.Snapshot, &snapshot) != nil {
		return
	}
	deadline := snapshot.ExpiresAt
	if !snapshot.Active {
		deadline = now.Add(5 * time.Second)
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		delete(r.guardGrants, u.Name)
		return
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		delete(r.guardGrants, u.Name)
		return
	}
	until := uint64(clock.BoottimeNanoseconds) + uint64(remaining)
	observed := uint64(clock.BoottimeNanoseconds)
	if old, ok := r.guardGrants[u.Name]; ok && old.revision == r.config.revision && old.generation == u.Generation && old.epoch == u.ProducerEpoch && old.bootID == clock.BootID && old.value == snapshot.Active && (old.deadline.Equal(deadline) || bytes.Equal(r.records[u.Name].Snapshot, u.Snapshot)) {
		until = old.untilNanos
		observed = old.observedNanos
		deadline = old.deadline
	}
	if r.guardGrants == nil {
		r.guardGrants = make(map[string]guardGrant)
	}
	r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: clock.BootID, value: snapshot.Active, deadline: deadline, untilNanos: until, observedNanos: observed}

}

// conditionFactsLocked adapts already validated protocol messages and a fresh
// supervisor observation into one finite fact snapshot. Workers cannot select
// their keys, activate producers, or publish generic arbitrary facts.
func (r *Runtime) conditionFactsLocked(cfg configuration, now time.Time) (result resourceprogram.FactSnapshot, resultErr error) {
	clock, err := bfd.ReadClock()
	if err != nil {
		return resourceprogram.FactSnapshot{}, err
	}
	scope := resourceprogram.FactScope{BootID: clock.BootID, ConfigDigest: cfg.digest, OperatorRevision: cfg.revision, ProjectionGeneration: r.projectionGeneration}
	ticks := uint64(clock.BoottimeNanoseconds)
	facts := resourceprogram.FactSnapshot{Scope: scope, NowNanos: ticks, WallTime: clock.WallTime}
	if cfg.revision != r.config.revision || cfg.digest != r.config.digest {
		return facts, nil
	}
	defer func() {
		if resultErr == nil {
			resultErr = r.advanceVRRPTracksLocked(result)
		}
		if resultErr == nil {
			resultErr = r.observeEventRulesLocked(result)
		}
		if resultErr == nil {
			resultErr = r.observeDerivedLocked(result)
		}
	}()
	if !now.Before(r.guardUntil) {
		return facts, nil
	}
	if r.guardBootID != clock.BootID || ticks >= r.guardUntilNanos {
		return facts, nil
	}
	facts.Facts = make(map[string]resourceprogram.Fact, 2*len(r.bindings))
	for name, binding := range r.bindings {
		module, ok := r.guardStatus[name]
		if !ok || module.Generation == 0 || module.ConfigDigest != binding.digest {
			continue
		}
		ready := resourceprogram.False
		if module.Ready {
			ready = resourceprogram.True
		}
		facts.Facts[name+".ready"] = resourceprogram.Fact{Value: ready, ObservedNanos: r.guardUntilNanos - uint64(time.Second), ExpiresNanos: r.guardUntilNanos, WorkerGeneration: module.Generation, ProducerEpoch: r.supervisorInstance}
		record := r.records[name]
		if record.Role == "dhcp_server" {
			s := r.dhcpServers[name]
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || !dhcpserverconfig.CurrentObservation(s, clock) {
				continue
			}
			value := resourceprogram.False
			if s.Active {
				value = resourceprogram.True
			}
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: s.ObservedNanos, ExpiresNanos: min(s.ExpiresNanos, r.guardUntilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			continue
		}
		if record.Role == "vrrp" {
			var election vrrp.Snapshot
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || json.Unmarshal(record.Snapshot, &election) != nil || election.BootID != clock.BootID {
				continue
			}
			fact := resourceprogram.Fact{Value: resourceprogram.False, ObservedNanos: election.ObservedNanos, ExpiresNanos: min(election.ExpiresNanos, r.guardUntilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			if r.ownsVRRPLocked(record, election) {
				fact.Value = resourceprogram.True
			}
			facts.Facts[name+".active"] = fact
			fact.Value = resourceprogram.False
			if election.State == vrrp.StateActive {
				fact.Value = resourceprogram.True
			}
			facts.Facts[name+".elected"] = fact
			continue
		}
		grant, ok := r.guardGrants[name]
		if !ok || !module.Ready || grant.revision != cfg.revision || grant.bootID != clock.BootID || grant.generation != module.Generation || grant.epoch != record.ProducerEpoch || grant.sequence != record.Sequence {
			continue
		}
		value := resourceprogram.False
		if grant.value {
			value = resourceprogram.True
		}
		facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: grant.observedNanos, ExpiresNanos: min(grant.untilNanos, r.guardUntilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
	}
	return r.conditionTimeline.Observe(cfg.source, facts)
}

func (r *Runtime) projectProgramLocked(cfg configuration, records map[string]serviceconfig.Update, now time.Time) (projection, error) {
	// Preserve the existing static fast path. Runtime readiness edges are opt-in
	// through ready_when; process readiness is not protocol readiness.
	if !dynamicProgram(cfg.source) {
		p, err := r.projectLocked(cfg.plan.Native, records, now, resourceprogram.FactSnapshot{})
		p.observationVersion = r.observationVersion
		p.resources = make(map[string]resourceprogram.ResourceState, len(cfg.source.Resources))
		for _, resource := range cfg.source.Resources {
			p.resources[resource.ID] = resourceprogram.ResourceState{Active: true, Ready: resourceprogram.True, When: resourceprogram.True}
		}
		return p, err
	}
	facts, err := r.conditionFactsLocked(cfg, now)
	if err != nil {
		return projection{}, err
	}
	native, states := cfg.plan.Project(facts)
	p, err := r.projectLocked(native, records, now, facts)
	p.observationVersion = r.observationVersion
	p.resources = states
	return p, err
}
