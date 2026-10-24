package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestEventRuleLifetimeSeesFlapBetweenControllerPolls(t *testing.T) {
	ctx := context.Background()
	r, _, _ := conditionalFixture(t)
	source := conditionSource()
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "debounce", Topic: "fixture.input", Duration: "1s"}, Emit: api.EventRuleEmitSpec{Topic: "fixture.output"}}
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "rule", EventRule: &spec, When: &resourceprogram.FactCondition{Fact: "service.wan.active"}})
	if err := r.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	lease := lease4(time.Now().Add(time.Hour))
	seq := uint64(0)
	publish := func(active bool) {
		t.Helper()
		seq++
		lease.Active = active
		if err := r.Accept(ctx, update(t, r, "service.wan", seq, lease)); err != nil {
			t.Fatal(err)
		}
	}
	reconcileConditions(t, r)
	publish(true)
	before, err := r.ActiveEventRules(source, 2)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	publish(false)
	publish(true)
	after, err := r.ActiveEventRules(source, 2)
	if err != nil || len(after) != 1 {
		t.Fatal(after, err)
	}
	if before[0].Identity == after[0].Identity {
		t.Fatal("rule inherited pending timer across an unpolled inactive interval")
	}
}
