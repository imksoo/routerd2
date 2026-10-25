package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
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
		lease = lease4(time.Now().Add(time.Hour))
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

func TestUnconditionalHistoricalRuleDoesNotDependOnUnrelatedWorkerReadiness(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &memoryManager{}
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "configuration.committed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "fixture.output"}}
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "rule", EventRule: &spec}}}
	if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.phase = "degraded"
	r.lastError = "unrelated logging worker is restarting"
	r.mu.Unlock()
	rules, err := r.ActiveEventRules(source, 1)
	if err != nil || len(rules) != 1 {
		t.Fatal("unrelated readiness revoked durable historical consumer", rules, err)
	}
}
