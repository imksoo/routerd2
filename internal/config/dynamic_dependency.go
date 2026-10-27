package config

import "github.com/imksoo/routerd2/internal/resourceprogram"

// A worker may use a protocol-owned link only when the source graph ties its
// lifetime to that owner and requires current positive ownership. Process
// readiness alone does not establish an elected VIP or a negotiated PPP unit.
func guardedDynamicResource(consumer, owner *resourceprogram.Resource) bool {
	if owner.Service.VRRP == nil && owner.Service.PPPoE == nil && owner.Service.DSLite == nil {
		return false
	}
	edge := consumer.Owner == owner.ID
	for _, dependency := range consumer.DependsOn {
		edge = edge || dependency == owner.ID
	}
	fact := owner.Service.ModuleName() + ".active"
	return edge && (conditionRequiresFact(consumer.When, fact) || conditionRequiresFact(owner.ReadyWhen, fact))
}

// The compiler already validated expression shape/depth. Infer the necessary
// positive fact under Kleene three-valued logic. An unknown fact never grants
// ownership, including through negation. This also handles double negation and
// De Morgan forms without treating an unrelated disjunct as authorization.
func conditionRequiresFact(condition *resourceprogram.FactCondition, fact string) bool {
	return conditionResultRequiresFact(condition, fact, true)
}

func conditionResultRequiresFact(condition *resourceprogram.FactCondition, fact string, result bool) bool {
	if condition == nil {
		return false
	}
	if condition.Fact != "" {
		return result && condition.Fact == fact
	}
	if condition.Not != nil {
		return conditionResultRequiresFact(condition.Not, fact, !result)
	}
	children, allRequired := condition.All, !result
	if len(condition.Any) > 0 {
		children, allRequired = condition.Any, result
	}
	if len(children) == 0 {
		return false
	}
	for i := range children {
		requires := conditionResultRequiresFact(&children[i], fact, result)
		if allRequired && !requires {
			return false
		}
		if !allRequired && requires {
			return true
		}
	}
	return allRequired
}
