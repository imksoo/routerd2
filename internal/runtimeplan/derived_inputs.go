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
	refs := map[string]derived.Reference{}
	for _, declaration := range r.config.plan.Native.DerivedEvents {
		for _, ref := range declaration.References {
			refs[ref.Key()] = ref
		}
	}
	requested := make([]derived.Reference, 0, len(refs))
	for _, ref := range refs {
		requested = append(requested, ref)
	}
	clock, values, err := r.statusValuesLocked(facts, requested)
	if err != nil {
		return derived.Snapshot{}, err
	}
	if r.ruleIncarnation == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return derived.Snapshot{}, err
		}
		r.ruleIncarnation = hex.EncodeToString(id[:])
	}
	scope := identity(clock.BootID, r.supervisorInstance, r.ruleIncarnation, r.config.revision, r.config.digest, r.sourceEpoch)
	for key, value := range values {
		value.Scope = scope
		values[key] = value
	}
	return derived.NewSnapshot(identity(scope, values), scope, uint64(clock.BoottimeNanoseconds), clock.WallTime, values)
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
