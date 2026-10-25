package resourceprogram

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/network"
)

func guardedProgram() Program {
	return Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "wan", When: &FactCondition{Fact: "health"}, ReadyWhen: &FactCondition{Fact: "address"}, Interface: &network.Interface{Name: "wan0"}},
		{ID: "route", Owner: "wan", Route: &network.Route{Destination: "0.0.0.0/0", Interface: "wan0", Gateway: "192.0.2.1"}},
		{ID: "other", Interface: &network.Interface{Name: "other0"}},
	}}
}

func TestProjectionGuardReadinessAndOwnerClosure(t *testing.T) {
	p := guardedProgram()
	before, _ := json.Marshal(p)
	tests := []struct {
		name       string
		values     map[string]Truth
		wan, route bool
		ready      Truth
	}{
		{"unknown", nil, false, false, Indeterminate},
		{"observed-down", map[string]Truth{"health": False}, false, false, False},
		{"start-before-ready", map[string]Truth{"health": True}, true, false, Indeterminate},
		{"ready", map[string]Truth{"health": True, "address": True}, true, true, True},
		{"readiness-lost", map[string]Truth{"health": True, "address": False}, true, false, False},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, states, err := projectSource(p, truthSnapshot(tt.values))
			if err != nil {
				t.Fatal(err)
			}
			if states["wan"].Active != tt.wan || states["route"].Active != tt.route || states["wan"].Ready != tt.ready || !states["other"].Active {
				t.Fatalf("unexpected states: %+v", states)
			}
			if (len(plan.Network.Routes) == 1) != tt.route {
				t.Fatalf("route projection: %+v", plan)
			}
		})
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("mutated durable source")
	}
}

func TestProjectionExplainsInactiveAndUnreadyResources(t *testing.T) {
	for _, tc := range []struct {
		values     map[string]Truth
		wan, route string
	}{
		{nil, "when_indeterminate", "dependency_indeterminate"},
		{map[string]Truth{"health": False}, "when_false", "dependency_false"},
		{map[string]Truth{"health": True}, "ready_when_indeterminate", "dependency_indeterminate"},
		{map[string]Truth{"health": True, "address": False}, "ready_when_false", "dependency_false"},
		{map[string]Truth{"health": True, "address": True}, "", ""},
	} {
		_, states, err := projectSource(guardedProgram(), truthSnapshot(tc.values))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(states)
		var view map[string]struct{ Reason, Dependency string }
		if err := json.Unmarshal(encoded, &view); err != nil {
			t.Fatal(err)
		}
		if view["wan"].Reason != tc.wan || view["route"].Reason != tc.route || tc.route != "" && view["route"].Dependency != "wan" || view["other"].Reason != "" {
			t.Fatalf("unexplained projection: %s", encoded)
		}
	}
}

func TestProjectionValidatesInactiveDeclarations(t *testing.T) {
	p := guardedProgram()
	p.Resources[0].When = &FactCondition{Fact: "health", All: []FactCondition{}}
	if _, _, err := projectSource(p, FactSnapshot{}); err == nil {
		t.Fatal("ambiguous empty operand lost by JSON cloning")
	}
	p = guardedProgram()
	p.Resources[1].Route.Destination = "invalid"
	if _, _, err := projectSource(p, FactSnapshot{}); err == nil {
		t.Fatal("inactive invalid payload accepted")
	}
	p = guardedProgram()
	p.Resources[0].DependsOn = []string{"route"}
	if _, _, err := projectSource(p, FactSnapshot{}); err == nil {
		t.Fatal("inactive graph cycle accepted")
	}
}

func truthSnapshot(values map[string]Truth) FactSnapshot {
	facts := make(map[string]Fact, len(values))
	for key, value := range values {
		facts[key] = Fact{Value: value, ExpiresNanos: 10}
	}
	return FactSnapshot{Facts: facts, NowNanos: 9}
}

func TestCompiledProjectionReusesConfigurationAcrossObservationChanges(t *testing.T) {
	source := guardedProgram()
	compiled, err := Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []Truth{True, False, True} {
		native, states := compiled.Project(truthSnapshot(map[string]Truth{"health": value, "address": True}))
		if states["route"].Active != (value == True) || (len(native.Network.Routes) == 1) != (value == True) {
			t.Fatal("observation change lost the compiled dependency graph", value, states)
		}
		if len(native.Network.Routes) > 0 && native.Network.Routes[0].Destination != "0.0.0.0/0" {
			t.Fatal("observation change altered the configured destination")
		}
	}
}

func TestProjectionUsesOneFiniteSnapshot(t *testing.T) {
	snapshot := truthSnapshot(map[string]Truth{"health": True, "address": True})
	before, _, err := projectSource(guardedProgram(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	expiredView := snapshot
	expiredView.NowNanos = 10
	expired, states, err := projectSource(guardedProgram(), expiredView)
	if err != nil || len(expired.Network.Routes) != 0 || states["wan"].When != Indeterminate {
		t.Fatal("expiry authorized work", states, err)
	}
	after, _, err := projectSource(guardedProgram(), snapshot)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("later projection changed an earlier snapshot", err)
	}
}

func projectSource(source Program, facts FactSnapshot) (Native, map[string]ResourceState, error) {
	compiled, err := Compile(source)
	if err != nil {
		return Native{}, nil, err
	}
	projection, states := compiled.Project(facts)
	return projection, states, nil
}
