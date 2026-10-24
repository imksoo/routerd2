package resourceprogram

import (
	"testing"

	"github.com/imksoo/routerd/pkg/api"
)

func TestEventRuleCompilesAndProjectsWithItsResourceLifecycle(t *testing.T) {
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "configuration.committed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.commits", Attributes: map[string]string{"count": "${count}"}}}
	source := Program{SchemaVersion: SchemaVersion, Resources: []Resource{{ID: "commits", EventRule: &spec, When: &FactCondition{Fact: "service.health-wan.active"}}}}
	p, err := Compile(source)
	if err != nil || len(p.Native.EventRules) != 1 || p.Native.EventRules[0].ID != "commits" || p.Native.EventRules[0].Identity == "" {
		t.Fatal(p, err)
	}
	spec.Emit.Attributes["count"] = "changed"
	if p.Native.EventRules[0].Spec.Emit.Attributes["count"] != "${count}" {
		t.Fatal("compiled spec retained caller map")
	}
	projection, states, err := Project(source, FactSnapshot{})
	if err != nil || len(projection.Native.EventRules) != 0 || states["commits"].Active {
		t.Fatal(projection, states, err)
	}
	source.Resources[0].EventRule.Pattern.Operator = "unknown"
	if _, _, err = Project(source, FactSnapshot{}); err == nil {
		t.Fatal("inactive invalid event rule was accepted")
	}
}
