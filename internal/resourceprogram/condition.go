package resourceprogram

import "github.com/imksoo/routerd2/internal/factcondition"

// FactCondition shares the same bounded expression grammar with protocol
// admission policy. Evaluation remains owned by the resource coordinator.
type FactCondition = factcondition.Expression

func ValidateFactCondition(c FactCondition) error { return factcondition.Validate(c) }

// EvaluateCondition uses a single fact-store read lock for the entire tree:
// concurrent worker updates cannot mix multiple projection snapshots within
// one readiness decision. Missing or expired facts remain indeterminate.
// Holds require ConditionTimeline.Observe before evaluating the snapshot;
// this stateless convenience method leaves unevaluated holds indeterminate.
func (s *FactStore) EvaluateCondition(c FactCondition, nowNanos uint64) (Truth, error) {
	if err := ValidateFactCondition(c); err != nil {
		return Indeterminate, err
	}
	return s.Snapshot(nowNanos).evaluate(&c), nil
}
