package runtimeplan

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

type derivedContinuity struct {
	proof                 derived.InputProof
	epoch, since, lastNow uint64
}

func identity(parts ...any) string {
	body, _ := json.Marshal(parts)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (r *Runtime) rawDerivedSnapshotLocked(facts resourceprogram.FactSnapshot) (derived.Snapshot, error) {
	// BFD publishes independently of mu. Capture its immutable evidence before
	// the clock, so a newer receive cannot appear to come from the future after
	// waiting for the cache lock. Never hold bfdMu during clock I/O/evaluation.
	r.bfdMu.Lock()
	bfdSources := make(map[string]bfdBinding, len(r.bfdSources))
	for name, owner := range r.bfdSources {
		bfdSources[name] = owner
	}
	r.bfdMu.Unlock()
	clock, err := bfd.ReadClock()
	if err != nil {
		return derived.Snapshot{}, err
	}
	if r.ruleIncarnation == "" {
		var id [16]byte
		if _, err = rand.Read(id[:]); err != nil {
			return derived.Snapshot{}, err
		}
		r.ruleIncarnation = hex.EncodeToString(id[:])
	}
	now := uint64(clock.BoottimeNanoseconds)
	scope := identity(clock.BootID, r.supervisorInstance, r.ruleIncarnation, r.config.revision, r.config.digest, r.sourceEpoch)
	lifecycles := r.config.plan.Lifecycle(facts)
	refs := map[string]derived.Reference{}
	for _, declaration := range r.config.plan.Native.DerivedEvents {
		for _, ref := range declaration.References {
			refs[ref.Key()] = ref
		}
	}
	byID := map[string]resourceprogram.Resource{}
	for _, resource := range r.config.source.Resources {
		byID[resource.ID] = resource
	}
	type observed struct {
		known       bool
		snapshot    health.Snapshot
		bfdSnapshot bfd.Snapshot
		bfdConfig   bfd.Config
		isBFD       bool
		isBGP       bool
		bgpSnapshot bgpconfig.Snapshot
		bgpConfig   bgpconfig.Config
		unresolved  int
		origin      string
		until       uint64
	}
	providers := map[string]observed{}
	values := map[string]derived.Value{}
	for key, ref := range refs {
		provider, loaded := providers[ref.Resource]
		if !loaded {
			target := byID[ref.Resource]
			if target.Service != nil && target.Service.Health != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guardStatus[name]
				record := r.records[name]
				grant, granted := r.guardGrants[name]
				if exists && managed && granted && module.Ready && module.Generation == record.Generation && module.Generation == grant.generation && module.ConfigDigest == binding.digest && record.ConfigDigest == binding.digest && grant.revision == r.config.revision && grant.epoch == record.ProducerEpoch && grant.sequence == record.Sequence && clock.BootID == r.guardBootID && clock.BootID == grant.bootID && now < r.guardUntilNanos {
					if json.Unmarshal(record.Snapshot, &provider.snapshot) == nil && !provider.snapshot.Stopped && provider.snapshot.BootID == clock.BootID {
						provider.until = min(provider.snapshot.ExpiresNanos, grant.untilNanos, r.guardUntilNanos)
						provider.known = provider.snapshot.ObservedNanos <= now && provider.until > now && provider.until > provider.snapshot.ObservedNanos
						provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch)
					}
				}
			}
			if target.Service != nil && target.Service.BFD != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guardStatus[name]
				owner, cached := bfdSources[name]
				record := owner.source.update
				if exists && cached && owner.source.valid && managed && module.Ready && module.Generation == record.Generation && module.ConfigDigest == binding.digest && record.ConfigDigest == binding.digest && owner.binding.digest == binding.digest && clock.BootID == r.guardBootID && now < r.guardUntilNanos {
					provider.isBFD = true
					provider.bfdConfig = owner.source.config
					provider.bfdSnapshot = owner.source.snapshot
					provider.until = r.guardUntilNanos
					provider.known = true
					provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch)
				}
			}
			if target.Service != nil && target.Service.BGP != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guardStatus[name]
				record := r.records[name]
				grant, granted := r.guardGrants[name]
				applied, matched := r.bgpApplied[name]
				if exists && managed && granted && grant.bgp != nil && matched && r.applied.hash != "" && module.Ready && module.Generation == record.Generation && grant.generation == record.Generation && record.ConfigDigest == binding.digest && module.ConfigDigest == binding.digest && grant.revision == r.config.revision && grant.epoch == record.ProducerEpoch && grant.sequence == record.Sequence && clock.BootID == r.guardBootID && clock.BootID == grant.bootID && now < r.guardUntilNanos && applied.identity == grant.bgp.projection {
					provider.isBGP, provider.known = true, true
					provider.bgpSnapshot = grant.bgp.snapshot
					provider.bgpConfig = target.Service.BGPConfig(r.dir, r.stateDir)
					provider.unresolved = applied.unresolved
					provider.until = min(grant.untilNanos, r.guardUntilNanos)
					provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch, provider.bgpSnapshot.Epoch)
				}
			}
			providers[ref.Resource] = provider
		}
		value := derived.Value{Scope: scope}
		if provider.known && provider.isBGP {
			field, known := bgpconfig.CurrentStatus(provider.bgpConfig, provider.bgpSnapshot, ref.Field, clock, provider.unresolved)
			value = derived.Value{Text: field.Text, Known: known, Scope: scope, Origin: provider.origin, ObservedNanos: field.ObservedNanos, ExpiresNanos: min(field.ExpiresNanos, provider.until)}
		} else if provider.known && provider.isBFD {
			field, known := bfd.CurrentStatus(provider.bfdConfig, provider.bfdSnapshot, ref.Field, clock)
			value = derived.Value{Text: field.Text, Known: known, Scope: scope, Origin: provider.origin, Continuity: field.Continuity, EqualityContinuity: field.EqualityContinuity, ObservedNanos: field.ObservedNanos, ExpiresNanos: min(field.ExpiresNanos, provider.until)}
		} else if provider.known {
			text, known := health.ScalarStatus(provider.snapshot, ref.Field)
			value = derived.Value{Text: text, Known: known, Scope: scope, Origin: provider.origin, ObservedNanos: provider.snapshot.ObservedNanos, ExpiresNanos: provider.until}
		}
		values[key] = value
	}
	return derived.NewSnapshot(identity(scope, values), scope, now, time.Now(), values)
}

// Observe every accepted input and supervisor transition. This certifies
// predicate continuity between sampled reconciliations, without restoring
// historical values or depending on the lossy logging/event transport.
func (r *Runtime) observeDerivedLocked(facts resourceprogram.FactSnapshot) error {
	if len(r.config.plan.Native.DerivedEvents) == 0 {
		r.derivedContinuity = nil
		return nil
	}
	snapshot, err := r.rawDerivedSnapshotLocked(facts)
	if err != nil {
		return err
	}
	_, now := snapshot.Timebase()
	next := map[string]derivedContinuity{}
	for _, declaration := range r.config.plan.Native.DerivedEvents {
		proof := declaration.Compiled.Proof(snapshot)
		old := r.derivedContinuity[declaration.ID]
		if old.epoch == 0 || old.proof.Value != proof.Value || old.proof.Authority != proof.Authority || now < old.lastNow || old.proof.Value != derived.Indeterminate && now >= old.proof.UntilNanos {
			if old.epoch == math.MaxUint64 {
				return errors.New("derived input continuity exhausted")
			}
			old.epoch++
			old.since = now
		}
		old.proof = proof
		old.lastNow = now
		next[declaration.ID] = old
	}
	r.derivedContinuity = next
	return nil
}

func (r *Runtime) DerivedInputs(revision uint64) ([]resourceprogram.DerivedEvent, map[string]derived.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured || r.config.revision != revision {
		return nil, nil, nil
	}
	if len(r.config.plan.Native.DerivedEvents) == 0 {
		return nil, nil, nil
	}
	facts, err := r.conditionFactsLocked(r.config, time.Now())
	if err != nil {
		return nil, nil, err
	}
	native, _ := r.config.plan.Project(facts)
	snapshot, err := r.rawDerivedSnapshotLocked(facts)
	if err != nil {
		return nil, nil, err
	}
	views := map[string]derived.Snapshot{}
	for i := range native.DerivedEvents {
		declaration := &native.DerivedEvents[i]
		lifecycle := r.ruleContinuity[declaration.ID]
		declaration.ActiveUntilNanos = lifecycle.Until
		if lifecycle.Conditional {
			declaration.Identity = identity(declaration.Identity, r.ruleIncarnation, lifecycle.epoch)
		}
		continuity := r.derivedContinuity[declaration.ID]
		view, err := snapshot.WithContinuity(identity(declaration.ID, continuity.epoch), continuity.since)
		if err != nil {
			return nil, nil, err
		}
		views[declaration.ID] = view
	}
	return native.DerivedEvents, views, nil
}
