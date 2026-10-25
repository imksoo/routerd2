package resourceprogram

import "time"

// FactSnapshot is an immutable, single-instant view. All resources in one
// projection see the same producer epochs, scope and expiration boundary.
type FactSnapshot struct {
	Scope    FactScope
	NowNanos uint64
	// Facts is owned by this snapshot. The caller transfers the map and
	// must not mutate it while any evaluation uses this view.
	Facts map[string]Fact
	held  map[string]timedTruth
}

func (s FactSnapshot) Value(key string) Truth {
	fact, ok := s.Facts[key]
	if !ok || s.NowNanos < fact.ObservedNanos || s.NowNanos >= fact.ExpiresNanos {
		return Indeterminate
	}
	return fact.Value
}

func (s FactSnapshot) evaluate(c *FactCondition) Truth {
	if c == nil {
		return True
	}
	if c.For != "" {
		return s.held[conditionIdentity(c)].value
	}
	if c.Fact != "" {
		if c.MaxAge != "" {
			age, err := time.ParseDuration(c.MaxAge)
			fact, ok := s.Facts[c.Fact]
			observed := fact.ObservedNanos
			if err != nil || age <= 0 || !ok || s.NowNanos < observed || s.NowNanos-observed > uint64(age) {
				return Indeterminate
			}
		}
		return s.Value(c.Fact)
	}
	if c.Not != nil {
		return s.evaluate(c.Not).Not()
	}
	operands := c.All
	if operands == nil {
		operands = c.Any
	}
	values := make([]Truth, len(operands))
	for i := range operands {
		values[i] = s.evaluate(&operands[i])
	}
	if c.All != nil {
		return All(values...)
	}
	return Any(values...)
}

// ResourceState describes the desired projection, not a kernel acknowledgement.
// ready_when gates dependents; it never prevents its own resource from starting.
type ResourceState struct {
	Active     bool   `json:"active"`
	Ready      Truth  `json:"ready"`
	When       Truth  `json:"when"`
	Reason     string `json:"reason,omitempty"`
	Dependency string `json:"dependency,omitempty"`
}

// Project validates every declaration, including inactive ones, before
// selecting a dependency-closed subset. A false or unknown guard withdraws the
// entire dependent subtree while keeping the durable source unchanged.
func Project(source Program, facts FactSnapshot) (Plan, map[string]ResourceState, error) {
	full, err := Compile(source)
	if err != nil {
		return Plan{}, nil, err
	}
	byID := make(map[string]Resource, len(source.Resources))
	for _, r := range source.Resources {
		byID[r.ID] = r
	}
	states := make(map[string]ResourceState, len(source.Resources))
	selected := Program{SchemaVersion: source.SchemaVersion}
	for _, id := range full.Order {
		r := byID[id]
		state := ResourceState{When: facts.evaluate(r.When)}
		gate := state.When
		if gate != True {
			state.Reason = conditionReason("when", gate)
			if facts.pendingHold(r.When) {
				state.Reason = "when_hold"
			}
		}
		for _, dep := range full.Dependencies[id] {
			gate = All(gate, states[dep].Ready)
			// Explain a false prerequisite ahead of an indeterminate one,
			// consistent with the three-valued conjunction. Own guards keep
			// precedence; dependency order is deterministic in the compiler.
			if state.When == True && states[dep].Ready != True && (state.Dependency == "" || state.Reason == "dependency_indeterminate" && states[dep].Ready == False) {
				state.Reason, state.Dependency = conditionReason("dependency", states[dep].Ready), dep
			}
		}
		state.Active = gate == True
		state.Ready = gate
		if state.Active {
			state.Ready = facts.evaluate(r.ReadyWhen)
			if state.Ready != True {
				state.Reason = conditionReason("ready_when", state.Ready)
				if facts.pendingHold(r.ReadyWhen) {
					state.Reason = "ready_when_hold"
				}
			}
			selected.Resources = append(selected.Resources, r)
		}
		states[id] = state
	}
	plan, err := compile(selected, byID)
	if err != nil {
		return Plan{}, nil, err
	}
	return plan, states, nil
}

func conditionReason(prefix string, value Truth) string {
	if value == False {
		return prefix + "_false"
	}
	return prefix + "_indeterminate"
}
