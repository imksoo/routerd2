package resourceprogram

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

// LifecycleEvidence is transient continuity metadata, not stored permission.
// ReadyWhen gates dependents but not the resource's own activation.
type LifecycleEvidence struct {
	Active      bool
	Conditional bool
	Until, Now  uint64
	Authority   string
}

func (s FactSnapshot) Lifecycle(program Program) (map[string]LifecycleEvidence, error) {
	plan, err := Compile(program)
	if err != nil {
		return nil, err
	}
	byID := map[string]Resource{}
	for _, r := range program.Resources {
		byID[r.ID] = r
	}
	active := map[string]LifecycleEvidence{}
	ready := map[string]LifecycleEvidence{}
	leaf := func(c *FactCondition) LifecycleEvidence {
		if c == nil {
			return LifecycleEvidence{Active: true, Until: math.MaxUint64, Now: s.nowNanos, Authority: "static"}
		}
		p := s.timedCondition(c, false)
		return LifecycleEvidence{Active: p.value == True, Conditional: true, Until: p.until, Now: s.nowNanos, Authority: p.authority}
	}
	join := func(parts []LifecycleEvidence) LifecycleEvidence {
		p := LifecycleEvidence{Active: true, Until: math.MaxUint64, Now: s.nowNanos}
		var authorities []string
		for _, part := range parts {
			p.Active = p.Active && part.Active
			p.Conditional = p.Conditional || part.Conditional
			p.Until = min(p.Until, part.Until)
			authorities = append(authorities, part.Authority)
		}
		scope := s.scope
		scope.ProjectionGeneration = 0
		body, _ := json.Marshal([]any{scope, authorities})
		sum := sha256.Sum256(body)
		p.Authority = hex.EncodeToString(sum[:])
		return p
	}
	for _, id := range plan.Order {
		parts := []LifecycleEvidence{leaf(byID[id].When)}
		for _, dep := range plan.Dependencies[id] {
			parts = append(parts, ready[dep])
		}
		active[id] = join(parts)
		ready[id] = join([]LifecycleEvidence{active[id], leaf(byID[id].ReadyWhen)})
	}
	return active, nil
}
