package runtimeplan

import (
	"context"
	"encoding/json"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
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
	dhcp4                          *dhcp4.Snapshot
	health                         *health.Snapshot
	dhcp6                          *dhcp6config.Snapshot
}

func dynamicProgram(source config.Document) bool {
	for _, r := range source.Resources {
		if r.Service != nil && r.Service.NTP != nil {
			for _, group := range r.Service.NTP.Sources {
				if group.When != nil {
					return true
				}
			}
		}
		if r.When != nil || r.ReadyWhen != nil || r.EgressRoute != nil || r.Service != nil && r.Service.VRRP != nil && r.Service.VRRP.GracefulActivation != nil {
			return true
		}
		if r.Address != nil && r.Address.Options != nil && !r.Address.Options.ValidUntil.IsZero() || r.Route != nil && r.Route.ValidUntil != (lifetime.Deadline{}) {
			return true
		}
	}
	return false
}

// Capture the original observation budget before transport or lock waits.
// An older concurrent query cannot overwrite a newer process-tree observation.
func (r *Runtime) supervisorStatus(ctx context.Context) (supervisor.Status, boottime.Clock, error) {
	clock, err := boottime.Read()
	if err != nil {
		return supervisor.Status{}, clock, err
	}
	status, err := r.manager.Status(ctx)
	return status, clock, err
}

func (r *Runtime) installGuardStatusLocked(status supervisor.Status, clock boottime.Clock, err error) {
	until := uint64(clock.BoottimeNanoseconds) + uint64(time.Second)
	if clock.BootID != "" && clock.BootID == r.guard.bootID && until <= r.guard.untilNanos {
		return
	}
	view := supervisorObservation{modules: make(map[string]supervisor.ModuleStatus), ready: err == nil && status.Ready, bootID: clock.BootID, untilNanos: until}
	if err == nil {
		for _, module := range status.Modules {
			view.modules[module.Name] = module
		}
	}
	// Unavailable observations remain unknown, with no renewal after a wait.
	r.guard = view
	if len(r.config.plan.Native.DerivedEvents) > 0 {
		_, _ = r.conditionFactsLocked(r.config)
	}
}

func (r *Runtime) recordGuardGrantLocked(u serviceconfig.Update) {
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
		r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: s.BootID, value: s.Active, observedNanos: observed, untilNanos: until, dhcpLease: s.LeaseClock, dhcp4: &s}
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
}

// conditionFactsLocked adapts already validated protocol messages and a fresh
// supervisor observation into one finite fact snapshot. Workers cannot select
// their keys, activate producers, or publish generic arbitrary facts.
func (r *Runtime) conditionFactsLocked(cfg configuration) (result resourceprogram.FactSnapshot, resultErr error) {
	clock, err := boottime.Read()
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
	if r.guard.bootID != clock.BootID || ticks >= r.guard.untilNanos {
		return facts, nil
	}
	facts.Facts = make(map[string]resourceprogram.Fact, 2*len(r.bindings))
	for name, binding := range r.bindings {
		module, ok := r.guard.modules[name]
		if !ok || module.Generation == 0 || module.ConfigDigest != binding.digest {
			continue
		}
		ready := resourceprogram.False
		if module.Ready {
			ready = resourceprogram.True
		}
		facts.Facts[name+".ready"] = resourceprogram.Fact{Value: ready, ObservedNanos: r.guard.untilNanos - uint64(time.Second), ExpiresNanos: r.guard.untilNanos, WorkerGeneration: module.Generation, ProducerEpoch: r.supervisorInstance}
		record := r.records[name]

		if record.Role == "tailscale" {
			var s tailscaleconfig.Snapshot
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || json.Unmarshal(record.Snapshot, &s) != nil || s.BootID != clock.BootID || s.ObservedNanos > ticks || s.ExpiresNanos <= ticks {
				continue
			}
			value := resourceprogram.False
			if s.BackendState == "Running" && s.Online {
				value = resourceprogram.True
			}
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: s.ObservedNanos, ExpiresNanos: min(s.ExpiresNanos, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			continue
		}
		if record.Role == "aftr" {
			var endpoint aftr.Snapshot
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || json.Unmarshal(record.Snapshot, &endpoint) != nil || endpoint.BootID != clock.BootID || endpoint.ObservedNanos > ticks {
				continue
			}
			value := resourceprogram.False
			if endpoint.Active && endpoint.Available && endpoint.ValidUntil.Live(clock) {
				value = resourceprogram.True
			}
			until := ticks + uint64(min(time.Second, endpoint.ValidUntil.Remaining(clock)))
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: endpoint.ObservedNanos, ExpiresNanos: min(until, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			continue
		}
		if record.Role == "pppoe" {
			var session pppoeconfig.Snapshot
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || json.Unmarshal(record.Snapshot, &session) != nil || session.ValidUntil.BootID != clock.BootID || session.ObservedNanos > ticks {
				continue
			}
			value := resourceprogram.False
			if session.Active {
				value = resourceprogram.True
			}
			// The worker already bounds this session observation. A status poll
			// must not replace its deadline with a new one-second kernel lease.
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: session.ObservedNanos, ExpiresNanos: session.ValidUntil.Nanos, WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			continue
		}
		if record.Role == "dhcp_server" {
			s := r.dhcpServers[name]
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || !dhcpserverconfig.CurrentObservation(s, clock) {
				continue
			}
			value := resourceprogram.False
			if s.Active {
				value = resourceprogram.True
			}
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: s.ObservedNanos, ExpiresNanos: min(s.ExpiresNanos, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			continue
		}
		if record.Role == "vrrp" {
			var election vrrp.Snapshot
			if !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || json.Unmarshal(record.Snapshot, &election) != nil || election.BootID != clock.BootID {
				continue
			}
			fact := resourceprogram.Fact{Value: resourceprogram.False, ObservedNanos: election.ObservedNanos, ExpiresNanos: min(election.ExpiresNanos, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
			if r.ownsVRRPLocked(record, election) {
				fact.Value = resourceprogram.True
			}
			facts.Facts[name+".active"] = fact
			fact.Value = resourceprogram.False
			if election.State == vrrp.StateActive {
				fact.Value = resourceprogram.True
			}
			facts.Facts[name+".elected"] = fact
			fact.Value = resourceprogram.False
			if election.State == vrrp.StateBackup {
				fact.Value = resourceprogram.True
			}
			facts.Facts[name+".backup"] = fact
			continue
		}
		if record.Role == "dhcp6" {
			source := r.guardGrants[name].dhcp6
			if source == nil || !module.Ready || record.Generation != module.Generation || record.ConfigDigest != binding.digest || source.BootID != clock.BootID || source.ObservedNanos > ticks {
				continue
			}
			active := source.Active(clock)
			until := source.ObservedNanos + uint64(5*time.Second)
			if active {
				deadline, _ := source.LeaseUntil()
				if source.State == "information" {
					deadline, _ = source.InformationUntil(clock)
				}
				until = deadline.Nanos
				if until == 0 {
					until = ^uint64(0)
				}
			}
			value := resourceprogram.False
			if active {
				value = resourceprogram.True
			}
			facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: source.ObservedNanos, ExpiresNanos: min(until, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
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
		facts.Facts[name+".active"] = resourceprogram.Fact{Value: value, ObservedNanos: grant.observedNanos, ExpiresNanos: min(grant.untilNanos, r.guard.untilNanos), WorkerGeneration: module.Generation, ProducerEpoch: record.ProducerEpoch}
	}
	return r.conditionTimeline.Observe(cfg.source, facts)
}

func (r *Runtime) projectProgramLocked(cfg configuration, records map[string]serviceconfig.Update, now time.Time) (projection, error) {
	// Preserve the existing static fast path. Runtime readiness edges are opt-in
	// through ready_when; process readiness is not protocol readiness.
	if !dynamicProgram(cfg.source) {
		p, err := r.projectLocked(cfg.protocols, cfg.plan.Native, records, now, resourceprogram.FactSnapshot{})
		p.observationVersion = r.observationVersion
		p.resources = make(map[string]resourceprogram.ResourceState, len(cfg.source.Resources))
		for _, resource := range cfg.source.Resources {
			p.resources[resource.ID] = resourceprogram.ResourceState{Active: true, Ready: resourceprogram.True, When: resourceprogram.True}
		}
		return p, err
	}
	facts, err := r.conditionFactsLocked(cfg)
	if err != nil {
		return projection{}, err
	}
	native, states := cfg.plan.Project(facts)
	p, err := r.projectLocked(cfg.protocols, native, records, now, facts)
	p.observationVersion = r.observationVersion
	p.resources = states
	return p, err
}
