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
	s := newTestStore(t)
	second := uint64(time.Second)
	if err := s.Publish(observation(1, second, 20*second, true), second); err != nil {
		t.Fatal(err)
	}
	var condition FactCondition
	if err := json.Unmarshal([]byte(`{"fact":"health/wan/up","max_age":"2s"}`), &condition); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now  uint64
		want Truth
	}{{second, True}, {3 * second, True}, {3*second + 1, Indeterminate}, {20 * second, Indeterminate}} {
		value, err := s.EvaluateCondition(condition, tc.now)
		if err != nil || value != tc.want {
			t.Fatalf("max_age at %d: got %v/%v, want %v", tc.now, value, err, tc.want)
		}
	}
	negated := FactCondition{Not: &condition}
	if value, err := s.EvaluateCondition(negated, 4*second); err != nil || value != Indeterminate {
		t.Fatalf("stale negation authorized work: %v/%v", value, err)
	}
	snapshot := s.Snapshot(4 * second)
	if err := s.Publish(observation(2, 4*second, 20*second, true), 4*second); err != nil {
		t.Fatal(err)
	}
	if snapshot.evaluate(&condition) != Indeterminate || s.Snapshot(4*second).evaluate(&condition) != True {
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
