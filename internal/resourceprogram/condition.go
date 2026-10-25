package resourceprogram

import "github.com/imksoo/routerd2/internal/factcondition"

// FactCondition shares the same bounded expression grammar with protocol
// admission policy. Evaluation remains owned by the resource coordinator.
type FactCondition = factcondition.Expression

func ValidateFactCondition(c FactCondition) error { return factcondition.Validate(c) }
