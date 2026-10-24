package runtimeplan

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

type ruleContinuity struct {
	resourceprogram.LifecycleEvidence
	epoch uint64
}

func (r *Runtime) observeEventRulesLocked(source config.Document, facts resourceprogram.FactSnapshot) error {
	hasRules := false
	for _, v := range source.Resources {
		if v.EventRule != nil {
			hasRules = true
			break
		}
	}
	if !hasRules {
		r.ruleContinuity = nil
		return nil
	}
	proofs, err := facts.Lifecycle(source)
	if err != nil {
		return err
	}
	if r.ruleContinuity == nil {
		r.ruleContinuity = map[string]ruleContinuity{}
	}
	next := map[string]ruleContinuity{}
	for _, v := range source.Resources {
		if v.EventRule == nil {
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
func (r *Runtime) ActiveEventRules(source config.Document, revision uint64) ([]resourceprogram.EventRule, error) {
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured || r.phase != "ready" || r.revision != revision || r.sourceDigest != hex.EncodeToString(sum[:]) {
		return nil, nil
	}
	facts, err := r.conditionFactsLocked(source, revision, time.Now())
	if err != nil {
		return nil, err
	}
	plan, _, err := resourceprogram.Project(source, facts)
	if err == nil {
		for i := range plan.Native.EventRules {
			rule := &plan.Native.EventRules[i]
			proof := r.ruleContinuity[rule.ID]
			if proof.Conditional {
				if r.ruleIncarnation == "" {
					var id [16]byte
					if _, err = rand.Read(id[:]); err != nil {
						return nil, err
					}
					r.ruleIncarnation = hex.EncodeToString(id[:])
				}
				body, _ := json.Marshal([]any{rule.Identity, r.ruleIncarnation, proof.epoch})
				sum := sha256.Sum256(body)
				rule.Identity = hex.EncodeToString(sum[:])
			}
		}
	}
	return plan.Native.EventRules, err
}
