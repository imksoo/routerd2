package resourceprogram

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// FactCondition is a typed boolean expression over finite protocol facts.
// Exactly one of Fact, All, Any or Not must be present. The ResourceProgram
// planner must not accept a when declaration until it uses this evaluator.
type FactCondition struct {
	Fact string          `json:"fact,omitempty"`
	All  []FactCondition `json:"all,omitempty"`
	Any  []FactCondition `json:"any,omitempty"`
	Not  *FactCondition  `json:"not,omitempty"`
}

const (
	maxFactConditionDepth = 32
	maxFactConditionNodes = 1024
	maxFactConditionWidth = 64
)

// ValidateFactCondition rejects empty, ambiguous, deeply nested and oversized
// expressions before evaluation. Malformed expressions fail closed.
func ValidateFactCondition(c FactCondition) error {
	visited := 0
	var walk func(FactCondition, int) error
	walk = func(node FactCondition, depth int) error {
		visited++
		if visited > maxFactConditionNodes || depth > maxFactConditionDepth {
			return errors.New("fact condition exceeds depth or node limit")
		}
		fields := 0
		if node.Fact != "" {
			fields++
			if len(node.Fact) > 128 || strings.TrimSpace(node.Fact) != node.Fact || strings.IndexFunc(node.Fact, unicode.IsSpace) >= 0 {
				return errors.New("invalid fact reference")
			}
		}
		if node.All != nil {
			fields++
			if len(node.All) == 0 || len(node.All) > maxFactConditionWidth {
				return errors.New("all condition requires 1..64 operands")
			}
		}
		if node.Any != nil {
			fields++
			if len(node.Any) == 0 || len(node.Any) > maxFactConditionWidth {
				return errors.New("any condition requires 1..64 operands")
			}
		}
		if node.Not != nil {
			fields++
		}
		if fields != 1 {
			return fmt.Errorf("fact condition requires exactly one expression, found %d", fields)
		}
		for _, operand := range node.All {
			if err := walk(operand, depth+1); err != nil {
				return err
			}
		}
		for _, operand := range node.Any {
			if err := walk(operand, depth+1); err != nil {
				return err
			}
		}
		if node.Not != nil {
			return walk(*node.Not, depth+1)
		}
		return nil
	}
	return walk(c, 0)
}

// EvaluateCondition uses a single fact-store read lock for the entire tree:
// concurrent worker updates cannot mix multiple projection snapshots within
// one readiness decision. Missing or expired facts remain indeterminate.
func (s *FactStore) EvaluateCondition(c FactCondition, nowNanos uint64) (Truth, error) {
	if err := ValidateFactCondition(c); err != nil {
		return Indeterminate, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var evaluate func(FactCondition) Truth
	evaluate = func(node FactCondition) Truth {
		if node.Fact != "" {
			return s.factValueLocked(node.Fact, nowNanos)
		}
		if node.Not != nil {
			return evaluate(*node.Not).Not()
		}
		if node.All != nil {
			values := make([]Truth, 0, len(node.All))
			for _, operand := range node.All {
				values = append(values, evaluate(operand))
			}
			return All(values...)
		}
		values := make([]Truth, 0, len(node.Any))
		for _, operand := range node.Any {
			values = append(values, evaluate(operand))
		}
		return Any(values...)
	}
	return evaluate(c), nil
}
