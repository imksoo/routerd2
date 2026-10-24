package resourceprogram

import (
	"testing"
)

func TestConditionValidationRejectsAmbiguousAndUnbounded(t *testing.T) {
	cases := []FactCondition{
		{},
		{Fact: "health/up", Not: &FactCondition{Fact: "health/down"}},
		{All: []FactCondition{}},
		{Any: []FactCondition{}},
		{Fact: "health /up"},
		{Fact: " health/up"},
		{Fact: string(make([]byte, 129))},
	}
	deep := FactCondition{Fact: "alive"}
	for range 34 {
		deep = FactCondition{Not: &deep}
	}
	cases = append(cases, deep)
	for i, tc := range cases {
		if err := ValidateFactCondition(tc); err == nil {
			t.Fatalf("malformed condition %d accepted", i)
		}
	}
	if err := ValidateFactCondition(FactCondition{Fact: "health/wan/up"}); err != nil {
		t.Fatal(err)
	}
}

func TestConditionEvaluationUnknownFalseTrue(t *testing.T) {
	s := newTestStore(t)
	unknown := FactCondition{Fact: "vrrp/role/master"}
	health := FactCondition{Fact: "health/wan/up"}
	if value, err := s.EvaluateCondition(FactCondition{Not: &unknown}, 100); err != nil || value != Indeterminate {
		t.Fatalf("unknown VRRP role must remain unknown: %v, %v", value, err)
	}
	if err := s.Publish(observation(1, 100, 150, false), 100); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		condition FactCondition
		want      Truth
	}{
		{FactCondition{All: []FactCondition{health, unknown}}, False},
		{FactCondition{Any: []FactCondition{health, unknown}}, Indeterminate},
		{FactCondition{Not: &health}, True},
		{FactCondition{Not: &unknown}, Indeterminate},
	}
	for _, tc := range checks {
		value, err := s.EvaluateCondition(tc.condition, 110)
		if err != nil || value != tc.want {
			t.Fatalf("Eval(%+v) = %v, %v; want %v", tc.condition, value, err, tc.want)
		}
	}
	if value, err := s.EvaluateCondition(FactCondition{All: []FactCondition{health, unknown}}, 150); err != nil || value != Indeterminate {
		t.Fatalf("expired condition = %v, %v", value, err)
	}
}

func TestConditionFailsClosedOnInvalidExpression(t *testing.T) {
	s := newTestStore(t)
	if err := s.Publish(observation(1, 100, 200, true), 100); err != nil {
		t.Fatal(err)
	}
	value, err := s.EvaluateCondition(FactCondition{All: []FactCondition{{Fact: "health/wan/up"}, {}}}, 101)
	if err == nil || value != Indeterminate {
		t.Fatalf("invalid expression authorized a resource: %v, %v", value, err)
	}
}
