package runtimeplan

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/resourceprogram"
)

type ruleContinuity struct {
	resourceprogram.LifecycleEvidence
	epoch uint64
}

func (r *Runtime) observeEventRulesLocked(facts resourceprogram.FactSnapshot) error {
	if len(r.config.plan.Native.EventRules)+len(r.config.plan.Native.DerivedEvents) == 0 {
		r.ruleContinuity = nil
		return nil
	}
	proofs := r.config.plan.Lifecycle(facts)
	if r.ruleContinuity == nil {
		r.ruleContinuity = map[string]ruleContinuity{}
	}
	next := map[string]ruleContinuity{}
	for _, v := range r.config.source.Resources {
		if v.EventRule == nil && v.DerivedEvent == nil {
			continue
		}
		proof := proofs[v.ID]
		old := r.ruleContinuity[v.ID]
		if old.epoch == 0 || old.Active != proof.Active || old.Authority != proof.Authority || old.Active && proof.Now >= old.Until {
			if old.epoch == math.MaxUint64 {
				return errors.New("event rule continuity exhausted")
			}
			old.epoch++
		}
		old.LifecycleEvidence = proof
		next[v.ID] = old
	}
	r.ruleContinuity = next
	return nil
}

// ActiveEventRules takes a fresh single-instant lifecycle projection for the
// exact authoritative source. It does not reuse yesterday's applied Ready or
// turn an event into a protocol lease. Caller fences source changes separately.
func (r *Runtime) ActiveEventRules(revision uint64) ([]resourceprogram.EventRule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured || r.config.revision != revision {
		return nil, nil
	}
	facts, err := r.conditionFactsLocked(r.config, time.Now())
	if err != nil {
		return nil, err
	}
	native, _ := r.config.plan.Project(facts)
	for i := range native.EventRules {
		rule := &native.EventRules[i]
		proof := r.ruleContinuity[rule.ID]
		if proof.Conditional {
			if r.ruleIncarnation == "" {
				var id [16]byte
				if _, err = rand.Read(id[:]); err != nil {
					return nil, err
				}
				r.ruleIncarnation = hex.EncodeToString(id[:])
			}
			body, _ := json.Marshal(struct {
				Identity, Incarnation string
				Epoch, SourceEpoch    uint64
			}{rule.Identity, r.ruleIncarnation, proof.epoch, r.sourceEpoch})
			sum := sha256.Sum256(body)
			rule.Identity = hex.EncodeToString(sum[:])
		}
	}
	return native.EventRules, nil
}
