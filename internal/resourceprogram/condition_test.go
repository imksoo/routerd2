package resourceprogram

import (
	"encoding/json"
	"testing"
	"time"
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

func TestConditionMaxAgeUsesObservationClockAndRemainsUnknownUnderNot(t *testing.T) {
	second := uint64(time.Second)
	fact := Fact{Value: True, ObservedNanos: second, ExpiresNanos: 20 * second}
	var condition FactCondition
	if err := json.Unmarshal([]byte(`{"fact":"health/wan/up","max_age":"2s"}`), &condition); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now  uint64
		want Truth
	}{{second, True}, {3 * second, True}, {3*second + 1, Indeterminate}, {20 * second, Indeterminate}} {
		value := healthFacts(tc.now, fact).evaluate(&condition)
		if value != tc.want {
			t.Fatalf("max_age at %d: got %v, want %v", tc.now, value, tc.want)
		}
	}
	negated := FactCondition{Not: &condition}
	if value := healthFacts(4*second, fact).evaluate(&negated); value != Indeterminate {
		t.Fatalf("stale negation authorized work: %v", value)
	}
	snapshot := healthFacts(4*second, fact)
	fact.ObservedNanos = 4 * second
	if snapshot.evaluate(&condition) != Indeterminate || healthFacts(4*second, fact).evaluate(&condition) != True {
		t.Fatal("max_age snapshot mixed old and renewed observations")
	}
}

func TestConditionMaxAgeValidation(t *testing.T) {
	for _, raw := range []string{
		`{"fact":"health/up","max_age":"0s"}`,
		`{"fact":"health/up","max_age":"-1s"}`,
		`{"fact":"health/up","max_age":"forever"}`,
		`{"fact":"health/up","max_age":"999999999999999999h"}`,
		`{"not":{"fact":"health/up"},"max_age":"1s"}`,
	} {
		var condition FactCondition
		if err := json.Unmarshal([]byte(raw), &condition); err != nil {
			t.Fatal(err)
		}
		if err := ValidateFactCondition(condition); err == nil {
			t.Fatal("invalid max_age accepted", raw)
		}
	}
}

func TestConditionEvaluationUnknownFalseTrue(t *testing.T) {
	unknown := FactCondition{Fact: "vrrp/role/master"}
	health := FactCondition{Fact: "health/wan/up"}
	fact := Fact{Value: False, ObservedNanos: 100, ExpiresNanos: 150}
	if value := healthFacts(100, fact).evaluate(&FactCondition{Not: &unknown}); value != Indeterminate {
		t.Fatalf("unknown VRRP role must remain unknown: %v", value)
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
		value := healthFacts(110, fact).evaluate(&tc.condition)
		if value != tc.want {
			t.Fatalf("Eval(%+v) = %v; want %v", tc.condition, value, tc.want)
		}
	}
	if value := healthFacts(150, fact).evaluate(&FactCondition{All: []FactCondition{health, unknown}}); value != Indeterminate {
		t.Fatalf("expired condition = %v", value)
	}
}
