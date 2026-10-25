// Package factcondition defines bounded boolean expressions over finite facts.
package factcondition

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Expression is a typed boolean expression over finite protocol facts.
// Exactly one of Fact, All, Any or Not must be present. The ResourceProgram
// planner must not accept a when declaration until it uses this evaluator.
type Expression struct {
	Fact   string       `json:"fact,omitempty"`
	MaxAge string       `json:"max_age,omitempty"`
	For    string       `json:"for,omitempty"`
	All    []Expression `json:"all,omitempty"`
	Any    []Expression `json:"any,omitempty"`
	Not    *Expression  `json:"not,omitempty"`
}

const (
	maxExpressionDepth = 32
	maxExpressionNodes = 1024
	maxExpressionWidth = 64
)

// Validate rejects empty, ambiguous, deeply nested and oversized
// expressions before evaluation. Malformed expressions fail closed.
func Validate(c Expression) error {
	visited := 0
	var walk func(Expression, int) error
	walk = func(node Expression, depth int) error {
		visited++
		if visited > maxExpressionNodes || depth > maxExpressionDepth {
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
			if len(node.All) == 0 || len(node.All) > maxExpressionWidth {
				return errors.New("all condition requires 1..64 operands")
			}
		}
		if node.Any != nil {
			fields++
			if len(node.Any) == 0 || len(node.Any) > maxExpressionWidth {
				return errors.New("any condition requires 1..64 operands")
			}
		}
		if node.Not != nil {
			fields++
		}
		if fields != 1 {
			return fmt.Errorf("fact condition requires exactly one expression, found %d", fields)
		}
		if node.MaxAge != "" {
			age, err := time.ParseDuration(node.MaxAge)
			if node.Fact == "" || err != nil || age <= 0 {
				return errors.New("max_age requires a fact and a positive finite duration")
			}
		}
		if node.For != "" {
			hold, err := time.ParseDuration(node.For)
			if err != nil || hold <= 0 {
				return errors.New("for requires a positive finite duration")
			}
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
