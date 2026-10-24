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

func TestConditionHoldBoundaryRenewalFalseAndImmutableSnapshot(t *testing.T) {
	s := newTestStore(t)
	c := holdCondition(t, `{"fact":"health/wan/up","for":"1s","max_age":"2s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	sequence := uint64(0)
	publish := func(now uint64, value bool) {
		t.Helper()
		sequence++
		if err := s.Publish(observation(sequence, now, now+10*second, value), now); err != nil {
			t.Fatal(err)
		}
	}
	read := func(now uint64, want Truth) FactSnapshot {
		t.Helper()
		view, err := timeline.Observe(p, s.Snapshot(now))
		if err != nil || view.evaluate(&c) != want {
			t.Fatalf("hold at %d: got %v/%v, want %v", now, view.evaluate(&c), err, want)
		}
		return view
	}
	publish(second, true)
	pending := read(second, Indeterminate)
	if pending.evaluate(&FactCondition{Not: &c}) != Indeterminate {
		t.Fatal("negation authorized a pending hold")
	}
	publish(second+second/2, true)
	read(2*second-1, Indeterminate)
	read(2*second, True) // refresh did not restart the hold; no new event needed
	if pending.evaluate(&c) != Indeterminate {
		t.Fatal("later timer evaluation mutated a previous snapshot")
	}
	publish(2*second+1, false)
	read(2*second+1, False)
	publish(2*second+2, true)
	read(2*second+2, Indeterminate)
	read(3*second+2, True)
}

func TestConditionHoldResetsAcrossFreshnessGapAndEpoch(t *testing.T) {
	s := newTestStore(t)
	c := holdCondition(t, `{"fact":"health/wan/up","for":"1s","max_age":"2s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	observe := func(now uint64, want Truth) {
		t.Helper()
		view, err := timeline.Observe(p, s.Snapshot(now))
		if err != nil || view.evaluate(&c) != want {
			t.Fatalf("hold at %d: %v/%v, want %v", now, view.evaluate(&c), err, want)
		}
	}
	if err := s.Publish(observation(1, second, 20*second, true), second); err != nil {
		t.Fatal(err)
	}
	observe(second, Indeterminate)
	observe(2*second, True)
	// No evaluator call sees the stale interval. A fresh sample after that
	// interval must still restart continuity using the previous validity bound.
	if err := s.Publish(observation(2, 4*second, 20*second, true), 4*second); err != nil {
		t.Fatal(err)
	}
	observe(4*second, Indeterminate)
	observe(5*second, True)
	newEpoch := ProducerEpoch{Epoch: testEpoch.Epoch + 1, WorkerGeneration: 3}
	if err := s.ActivateProducer(testScope, "health/wan", newEpoch); err != nil {
		t.Fatal(err)
	}
	f := observation(1, 5*second+1, 20*second, true)
	f.Epoch = newEpoch
	if err := s.Publish(f, 5*second+1); err != nil {
		t.Fatal(err)
	}
	observe(5*second+1, Indeterminate)
	observe(6*second+1, True)
	observe(6*second, Indeterminate) // a backwards monotonic clock fails closed
}

func TestConditionHoldCompositeWitnessAndScope(t *testing.T) {
	s := newTestStore(t)
	c := holdCondition(t, `{"any":[{"fact":"health/wan/up"},{"fact":"unknown"}],"for":"1s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	if err := s.Publish(observation(1, second, 10*second, true), second); err != nil {
		t.Fatal(err)
	}
	for _, now := range []uint64{second, 2 * second} {
		view, err := timeline.Observe(p, s.Snapshot(now))
		want := Indeterminate
		if now == 2*second {
			want = True
		}
		if err != nil || view.evaluate(&c) != want {
			t.Fatal("unknown OR operand masked a valid witness", view.evaluate(&c), err)
		}
	}
	for _, change := range []func(*FactScope){
		func(s *FactScope) { s.BootID = "new-boot" },
		func(s *FactScope) { s.ConfigDigest = "new-source" },
		func(s *FactScope) { s.OperatorRevision++ },
	} {
		view := s.Snapshot(2 * second)
		change(&view.scope)
		held, err := timeline.Observe(p, view)
		if err != nil || held.evaluate(&c) != Indeterminate {
			t.Fatal("new scope reused old hold", err)
		}
	}
	// Successful network projection alone does not change the source authority.
	view := s.Snapshot(3 * second)
	view.scope.OperatorRevision++ // retain the last scope from the loop
	view.scope.ProjectionGeneration++
	held, err := timeline.Observe(p, view)
	if err != nil || held.evaluate(&c) != True {
		t.Fatal("projection generation restarted source hold", err)
	}
	if _, err := timeline.Observe(Program{}, view); err != nil || len(timeline.holds) != 0 {
		t.Fatal("removed conditions retained history", err)
	}
}

func TestConditionHoldValidation(t *testing.T) {
	for _, duration := range []string{"0s", "-1s", "forever", "999999999999999999999h"} {
		c := holdCondition(t, `{"fact":"health/wan/up","for":"`+duration+`"}`)
		if err := ValidateFactCondition(c); err == nil {
			t.Fatal("invalid hold accepted", duration)
		}
	}
}

func TestConditionHoldNestedAndChangingDisjunctionWitness(t *testing.T) {
	s := newTestStore(t)
	second := uint64(time.Second)
	sequence := uint64(0)
	publish := func(key string, now uint64, value bool) {
		t.Helper()
		sequence++
		f := observation(sequence, now, 10*second, value)
		f.Key = key
		if err := s.Publish(f, now); err != nil {
			t.Fatal(err)
		}
	}
	c := holdCondition(t, `{"any":[{"fact":"health/wan/up"},{"fact":"other"}],"for":"1s"}`)
	p := Program{Resources: []Resource{{When: &c}}}
	var timeline ConditionTimeline
	read := func(now uint64, want Truth) {
		t.Helper()
		view, err := timeline.Observe(p, s.Snapshot(now))
		if err != nil || view.evaluate(&c) != want {
			t.Fatal("continuous disjunction restarted or skipped its hold", view.evaluate(&c), err)
		}
	}
	publish("health/wan/up", second, true)
	publish("other", second, false)
	read(second, Indeterminate)
	publish("other", second+second/4, true)
	read(second+second/4, Indeterminate)
	publish("health/wan/up", second+second/2, false)
	read(second+second/2, Indeterminate)
	read(2*second, True)

	c = holdCondition(t, `{"not":{"not":{"fact":"other","for":"500ms"}},"for":"500ms"}`)
	p = Program{Resources: []Resource{{When: &c}}}
	timeline = ConditionTimeline{}
	read(2*second, Indeterminate)
	read(2*second+second/2, Indeterminate) // inner hold finishes; outer begins
	read(3*second, True)
}

func TestConditionHoldProducerExpiryAndProjectionReasons(t *testing.T) {
	s := newTestStore(t)
	c := holdCondition(t, `{"fact":"health/wan/up","for":"1s"}`)
	p := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "wan", When: &c, Interface: &network.Interface{Name: "wan0"}},
		{ID: "ready", ReadyWhen: &c, Interface: &network.Interface{Name: "lan0"}},
	}}
	var timeline ConditionTimeline
	second := uint64(time.Second)
	if err := s.Publish(observation(1, second, 2*second, true), second); err != nil {
		t.Fatal(err)
	}
	view, err := timeline.Observe(p, s.Snapshot(second))
	if err != nil {
		t.Fatal(err)
	}
	_, states, err := Project(p, view)
	if err != nil || states["wan"].Reason != "when_hold" || states["ready"].Reason != "ready_when_hold" || !states["ready"].Active {
		t.Fatal("pending holds lost their reason or gated their own readiness startup", states, err)
	}
	view, err = timeline.Observe(p, s.Snapshot(2*second))
	if err != nil || view.evaluate(&c) != Indeterminate || len(timeline.holds) != 0 {
		t.Fatal("expiry at hold boundary authorized work", err)
	}
	_, states, err = Project(p, view)
	if err != nil || states["wan"].Reason != "when_indeterminate" || states["ready"].Reason != "ready_when_indeterminate" {
		t.Fatal("stale observation was still explained as a hold", states, err)
	}
	if err := s.Publish(observation(2, 3*second, 10*second, true), 3*second); err != nil {
		t.Fatal(err)
	}
	view, err = timeline.Observe(p, s.Snapshot(3*second))
	if err != nil || view.evaluate(&c) != Indeterminate {
		t.Fatal("expired producer continuity survived a new observation", err)
	}
}
