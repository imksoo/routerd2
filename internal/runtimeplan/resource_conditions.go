package runtimeplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// guardGrant records a local monotonic deadline once at snapshot acceptance.
// Re-projecting, repeating a sequence or stepping wall time cannot renew it.
type guardGrant struct {
	revision, generation, sequence uint64
	epoch, bootID                  string
	value                          bool
	deadline                       time.Time
	untilNanos                     uint64
	observedNanos                  uint64
}

func conditionalProgram(source config.Document) bool {
	for _, r := range source.Resources {
		if r.When != nil || r.ReadyWhen != nil {
			return true
		}
	}
	return false
}

func (r *Runtime) refreshGuardStatus(ctx context.Context) {
	status, err := r.manager.Status(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installGuardStatusLocked(status, err)
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
}

func (r *Runtime) recordGuardGrantLocked(u serviceconfig.Update, now time.Time) {
	if u.Role == "health" {
		var snapshot health.Snapshot
		if json.Unmarshal(u.Snapshot, &snapshot) != nil {
			return
		}
		if r.guardGrants == nil {
			r.guardGrants = make(map[string]guardGrant)
		}
		r.guardGrants[u.Name] = guardGrant{revision: r.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: snapshot.BootID, value: snapshot.Active, untilNanos: snapshot.ExpiresNanos, observedNanos: snapshot.ObservedNanos}
		return
	}
	var snapshot struct {
		Active     bool      `json:"active"`
		ValidUntil time.Time `json:"valid_until"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
	switch u.Role {
	case "dhcp4", "dhcp6pd", "vrrp", "bgp", "dhcp_server":
	default:
		return
	}
	if json.Unmarshal(u.Snapshot, &snapshot) != nil {
		return
	}
	deadline := snapshot.ValidUntil
	if u.Role == "dhcp4" || u.Role == "dhcp6pd" {
		deadline = snapshot.ExpiresAt
	}
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
	if old, ok := r.guardGrants[u.Name]; ok && old.revision == r.revision && old.generation == u.Generation && old.epoch == u.ProducerEpoch && old.bootID == clock.BootID && old.value == snapshot.Active && (old.deadline.Equal(deadline) || bytes.Equal(r.records[u.Name].Snapshot, u.Snapshot)) {
		until = old.untilNanos
		observed = old.observedNanos
		deadline = old.deadline
	}
	if r.guardGrants == nil {
		r.guardGrants = make(map[string]guardGrant)
	}
	r.guardGrants[u.Name] = guardGrant{revision: r.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, bootID: clock.BootID, value: snapshot.Active, deadline: deadline, untilNanos: until, observedNanos: observed}

}

// conditionFactsLocked adapts already validated protocol messages and a fresh
// supervisor observation into one finite fact snapshot. Workers cannot select
// their keys, activate producers, or publish generic arbitrary facts.
func (r *Runtime) conditionFactsLocked(source config.Document, revision uint64, now time.Time) (result resourceprogram.FactSnapshot, resultErr error) {
	defer func() {
		if resultErr == nil {
			resultErr = r.observeEventRulesLocked(source, result)
		}
	}()
	if revision != r.revision || !now.Before(r.guardUntil) {
		return resourceprogram.FactSnapshot{}, nil
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		return resourceprogram.FactSnapshot{}, nil
	} // fail closed
	encoded, err := json.Marshal(source)
	if err != nil {
		return resourceprogram.FactSnapshot{}, err
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != r.sourceDigest {
		return resourceprogram.FactSnapshot{}, nil
	}
	scope := resourceprogram.FactScope{BootID: clock.BootID, ConfigDigest: hex.EncodeToString(sum[:]), OperatorRevision: revision, ProjectionGeneration: r.projectionGeneration}
	store, err := resourceprogram.NewFactStore(scope)
	if err != nil {
		return resourceprogram.FactSnapshot{}, err
	}
	ticks := uint64(clock.BoottimeNanoseconds)
	if r.guardBootID != clock.BootID || ticks >= r.guardUntilNanos {
		return resourceprogram.FactSnapshot{}, nil
	}
	for name, binding := range r.bindings {
		module, ok := r.guardStatus[name]
		if !ok || module.Generation == 0 || module.ConfigDigest != binding.digest {
			continue
		}
		epoch := resourceprogram.ProducerEpoch{Epoch: 1, WorkerGeneration: module.Generation}
		if err := store.ActivateProducer(scope, name, epoch); err != nil {
			return resourceprogram.FactSnapshot{}, err
		}
		publish := func(key string, sequence uint64, value bool, observed, until uint64) error {
			if until <= ticks {
				return nil
			}
			return store.Publish(resourceprogram.ProtocolFact{Key: key, Producer: name, Scope: scope, Epoch: epoch, Sequence: sequence, ObservedNanos: observed, ExpiresNanos: until, Value: value}, ticks)
		}
		if err := publish(name+".ready", 1, module.Ready, r.guardUntilNanos-uint64(time.Second), r.guardUntilNanos); err != nil {
			return resourceprogram.FactSnapshot{}, err
		}
		grant, ok := r.guardGrants[name]
		record := r.records[name]
		if !ok || !module.Ready || grant.revision != revision || grant.bootID != clock.BootID || grant.generation != module.Generation || grant.epoch != record.ProducerEpoch || grant.sequence != record.Sequence {
			continue
		}
		until := min(grant.untilNanos, r.guardUntilNanos)
		if err := publish(name+".active", 2, grant.value, grant.observedNanos, until); err != nil {
			return resourceprogram.FactSnapshot{}, err
		}
	}
	return r.conditionTimeline.Observe(source, store.Snapshot(ticks))
}

func (r *Runtime) projectProgramLocked(source config.Document, revision uint64, full config.Native, records map[string]serviceconfig.Update, now time.Time) (projection, error) {
	// Preserve the existing static fast path. Runtime readiness edges are opt-in
	// through ready_when; process readiness is not protocol readiness.
	if !conditionalProgram(source) {
		p, err := r.projectLocked(full, records, now)
		p.resources = make(map[string]resourceprogram.ResourceState, len(source.Resources))
		for _, resource := range source.Resources {
			p.resources[resource.ID] = resourceprogram.ResourceState{Active: true, Ready: resourceprogram.True, When: resourceprogram.True}
		}
		return p, err
	}
	facts, err := r.conditionFactsLocked(source, revision, now)
	if err != nil {
		return projection{}, err
	}
	plan, states, err := resourceprogram.Project(source, facts)
	if err != nil {
		return projection{}, err
	}
	p, err := r.projectLocked(plan.Native, records, now)
	p.resources = states
	return p, err
}
