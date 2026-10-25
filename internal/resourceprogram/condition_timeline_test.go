package resourceprogram

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/network"
)

func holdCondition(t *testing.T, raw string) FactCondition {
	t.Helper()
	var c FactCondition
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConditionHoldRetainsRenewalAndResetsOnFalseGapOrWorkerReplacement(t *testing.T) {
	c := holdCondition(t, `{"fact":"health/wan/up","for":"1s","max_age":"2s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	fact := Fact{Value: True, ObservedNanos: second, ExpiresNanos: 20 * second, WorkerGeneration: 1, ProducerEpoch: "first"}
	read := func(now uint64, want Truth) FactSnapshot {
		t.Helper()
		view, err := timeline.Observe(p, healthFacts(now, fact))
		if err != nil || view.evaluate(&c) != want {
			t.Fatalf("hold at %d: got %v/%v, want %v", now, view.evaluate(&c), err, want)
		}
		return view
	}
	pending := read(second, Indeterminate)
	if pending.evaluate(&FactCondition{Not: &c}) != Indeterminate {
		t.Fatal("negation authorized a pending hold")
	}
	fact.ObservedNanos = second + second/2
	read(2*second-1, Indeterminate)
	read(2*second, True)
	if pending.evaluate(&c) != Indeterminate {
		t.Fatal("later evaluation mutated a previous snapshot")
	}
	fact.Value, fact.ObservedNanos = False, 2*second+1
	read(2*second+1, False)
	fact.Value, fact.ObservedNanos = True, 2*second+2
	read(2*second+2, Indeterminate)
	read(3*second+2, True)
	// No evaluation sees the freshness gap. Renewal must still restart the hold.
	fact.ObservedNanos = 6 * second
	read(6*second, Indeterminate)
	read(7*second, True)
	fact.WorkerGeneration, fact.ProducerEpoch, fact.ObservedNanos = 2, "replacement", 7*second+1
	read(7*second+1, Indeterminate)
	read(8*second+1, True)
	read(8*second, Indeterminate)
}

func TestConditionHoldScopeAndExpiryAtAdmissionBoundary(t *testing.T) {
	c := holdCondition(t, `{"fact":"health/wan/up","for":"1s"}`)
	p := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "wan", When: &c, Interface: &network.Interface{Name: "wan0"}},
		{ID: "ready", ReadyWhen: &c, Interface: &network.Interface{Name: "lan0"}},
	}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	fact := Fact{Value: True, ObservedNanos: second, ExpiresNanos: 2 * second}
	view, err := timeline.Observe(p, healthFacts(second, fact))
	if err != nil {
		t.Fatal(err)
	}
	_, states, err := projectSource(p, view)
	if err != nil || states["wan"].Reason != "when_hold" || states["ready"].Reason != "ready_when_hold" || !states["ready"].Active {
		t.Fatal("hold prevented its own resource startup or lost its reason", states, err)
	}
	view, err = timeline.Observe(p, healthFacts(2*second, fact))
	if err != nil || view.evaluate(&c) != Indeterminate {
		t.Fatal("expired observation authorized work at the hold boundary", err)
	}
	fact.ObservedNanos, fact.ExpiresNanos = 3*second, 20*second
	view, err = timeline.Observe(p, healthFacts(3*second, fact))
	if err != nil || view.evaluate(&c) != Indeterminate {
		t.Fatal("expired continuity survived renewal", err)
	}
	view, err = timeline.Observe(p, healthFacts(4*second, fact))
	if err != nil || view.evaluate(&c) != True {
		t.Fatal(err)
	}
	view.Scope.OperatorRevision++
	view, err = timeline.Observe(p, view)
	if err != nil || view.evaluate(&c) != Indeterminate {
		t.Fatal("source revision reused an old hold", err)
	}
	view.NowNanos = 5 * second
	view.Scope.ProjectionGeneration++
	view, err = timeline.Observe(p, view)
	if err != nil || view.evaluate(&c) != True {
		t.Fatal("kernel projection restarted source continuity", err)
	}
}

func TestConditionHoldNestedAndChangingDisjunctionWitness(t *testing.T) {
	second := uint64(time.Second)
	facts := FactSnapshot{Scope: FactScope{BootID: "boot", ConfigDigest: "source"}, Facts: map[string]Fact{
		"health/wan/up": {Value: True, ObservedNanos: second, ExpiresNanos: 10 * second},
		"other":         {Value: False, ObservedNanos: second, ExpiresNanos: 10 * second},
	}}
	c := holdCondition(t, `{"any":[{"fact":"health/wan/up"},{"fact":"other"}],"for":"1s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	read := func(now uint64, want Truth) {
		t.Helper()
		facts.NowNanos = now
		view, err := timeline.Observe(p, facts)
		if err != nil || view.evaluate(&c) != want {
			t.Fatal("disjunction restarted or skipped its hold", view.evaluate(&c), err)
		}
	}
	read(second, Indeterminate)
	facts.Facts["other"] = Fact{Value: True, ObservedNanos: second + second/4, ExpiresNanos: 10 * second}
	read(second+second/4, Indeterminate)
	facts.Facts["health/wan/up"] = Fact{Value: False, ObservedNanos: second + second/2, ExpiresNanos: 10 * second}
	read(second+second/2, Indeterminate)
	read(2*second, True)
	c = holdCondition(t, `{"not":{"not":{"fact":"other","for":"500ms"}},"for":"500ms"}`)
	p = Program{Resources: []Resource{{When: &c}}}
	timeline = ConditionTimeline{}
	read(2*second, Indeterminate)
	read(2*second+second/2, Indeterminate)
	read(3*second, True)
}
