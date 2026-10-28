package resourceprogram

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"time"
)

type timedTruth struct {
	value     Truth
	until     uint64
	authority string
	holding   bool
}

type conditionHold struct {
	since, until uint64
	authority    string
}

// ConditionTimeline tracks continuity for the currently declared conditions,
// bounded by their node count rather than by the number of observations.
// The coordinator must serialize calls and observe every accepted transition,
// not just the snapshots selected for a kernel projection. State is transient:
// a new boot, source, operator revision or producer resets the affected hold.
type ConditionTimeline struct {
	scope   FactScope
	lastNow uint64
	holds   map[string]conditionHold
}

func conditionIdentity(c *FactCondition) string {
	b, _ := json.Marshal(c) // validated, acyclic expression tree
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Observe returns a new immutable snapshot including all declared hold results.
// Project remains pure; time and observation history are explicit inputs.
// A pending hold is indeterminate, including when negated. A known false input
// withdraws immediately. The duration boundary itself is inclusive.
func (t *ConditionTimeline) Observe(program Program, facts FactSnapshot) (FactSnapshot, error) {
	for _, resource := range program.Resources {
		for _, c := range resourceConditions(resource) {
			if c != nil {
				if err := ValidateFactCondition(*c); err != nil {
					return FactSnapshot{}, err
				}
			}
		}
	}
	scope := facts.Scope
	// A successful kernel projection must not restart observation continuity.
	// Its authority remains fenced separately by the observation coordinator.
	scope.ProjectionGeneration = 0
	if scope != t.scope || facts.NowNanos < t.lastNow {
		t.holds = nil
	}
	next := make(map[string]conditionHold)
	facts.held = make(map[string]timedTruth)
	var visit func(*FactCondition)
	visit = func(c *FactCondition) {
		if c == nil {
			return
		}
		for i := range c.All {
			visit(&c.All[i])
		}
		for i := range c.Any {
			visit(&c.Any[i])
		}
		visit(c.Not)
		if c.For == "" {
			return
		}
		key := conditionIdentity(c)
		raw := facts.timedCondition(c, true)
		result := raw
		if raw.value == True {
			hold, exists := t.holds[key]
			if !exists || facts.NowNanos >= hold.until || hold.authority != raw.authority {
				hold.since = facts.NowNanos
			}
			hold.until, hold.authority = raw.until, raw.authority
			next[key] = hold
			duration, _ := time.ParseDuration(c.For) // validated above
			if facts.NowNanos-hold.since < uint64(duration) {
				result.value = Indeterminate
				result.holding = true
			}
		}
		facts.held[key] = result
	}
	for _, resource := range program.Resources {
		for _, condition := range resourceConditions(resource) {
			visit(condition)
		}
	}
	t.scope, t.lastNow, t.holds = scope, facts.NowNanos, next
	return facts, nil
}

func (s FactSnapshot) pendingHold(c *FactCondition) bool {
	if c == nil || s.evaluate(c) != Indeterminate {
		return false
	}
	if c.For != "" && s.held[conditionIdentity(c)].holding {
		return true
	}
	if s.pendingHold(c.Not) {
		return true
	}
	for i := range c.All {
		if s.pendingHold(&c.All[i]) {
			return true
		}
	}
	for i := range c.Any {
		if s.pendingHold(&c.Any[i]) {
			return true
		}
	}
	return false
}

func (s FactSnapshot) timedCondition(c *FactCondition, ignoreOwnHold bool) timedTruth {
	if c.For != "" && !ignoreOwnHold {
		return s.held[conditionIdentity(c)]
	}
	if c.Fact != "" {
		leaf := *c
		leaf.For = ""
		fact := s.Facts[c.Fact]
		origin := strconv.FormatUint(fact.WorkerGeneration, 10) + ":" + fact.ProducerEpoch
		result := timedTruth{value: s.evaluate(&leaf), until: fact.ExpiresNanos, authority: origin}
		if c.MaxAge != "" {
			age, _ := time.ParseDuration(c.MaxAge)
			observed := fact.ObservedNanos
			deadline := uint64(math.MaxUint64)
			if uint64(age) < math.MaxUint64-observed {
				deadline = observed + uint64(age) + 1 // max_age boundary is valid
			}
			result.until = min(result.until, deadline)
		}
		if result.until <= s.NowNanos {
			result.value = Indeterminate
		}
		return result
	}
	if c.Not != nil {
		result := s.timedCondition(c.Not, false)
		result.value = result.value.Not()
		return result
	}
	operands := c.All
	if operands == nil {
		operands = c.Any
	}
	children := make([]timedTruth, len(operands))
	values := make([]Truth, len(operands))
	origins := make([]string, len(operands))
	for i := range operands {
		children[i] = s.timedCondition(&operands[i], false)
		values[i], origins[i] = children[i].value, children[i].authority
	}
	result := timedTruth{value: Any(values...)}
	if c.All != nil {
		result.value = All(values...)
	}
	// Known conjunction-false or disjunction-true needs only one witness.
	// Its result remains known while any such witness is still valid.
	witness := c.All != nil && result.value == False || c.Any != nil && result.value == True
	if witness {
		for _, child := range children {
			if child.value == result.value {
				result.until = max(result.until, child.until)
			}
		}
	} else if result.value != Indeterminate {
		result.until = math.MaxUint64
		for _, child := range children {
			result.until = min(result.until, child.until)
		}
	}
	b, _ := json.Marshal(origins)
	sum := sha256.Sum256(b)
	result.authority = hex.EncodeToString(sum[:])
	return result
}

func resourceConditions(resource Resource) []*FactCondition {
	conditions := []*FactCondition{resource.When, resource.ReadyWhen}
	if resource.EgressRoute != nil {
		for _, candidate := range resource.EgressRoute.Candidates {
			conditions = append(conditions, candidate.When)
		}
	}
	if resource.Service != nil && resource.Service.VRRP != nil && resource.Service.VRRP.GracefulActivation != nil {
		conditions = append(conditions, &resource.Service.VRRP.GracefulActivation.ReadyWhen)
	}
	return conditions
}

// ConditionAuthority returns the current result and the original BOOTTIME
// boundary of its supporting facts. It cannot manufacture a new lifetime.
func (s FactSnapshot) ConditionAuthority(c FactCondition) (Truth, uint64) {
	result := s.timedCondition(&c, false)
	return result.value, result.until
}
