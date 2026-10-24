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
			plan, states, err := Project(p, FactSnapshot{values: tt.values})
			if err != nil {
				t.Fatal(err)
			}
			if states["wan"].Active != tt.wan || states["route"].Active != tt.route || states["wan"].Ready != tt.ready || !states["other"].Active {
				t.Fatalf("unexpected states: %+v", states)
			}
			if (len(plan.Native.Network.Routes) == 1) != tt.route {
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
		_, states, err := Project(guardedProgram(), FactSnapshot{values: tc.values})
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
	if _, _, err := Project(p, FactSnapshot{}); err == nil {
		t.Fatal("ambiguous empty operand lost by JSON cloning")
	}
	p = guardedProgram()
	p.Resources[1].Route.Destination = "invalid"
	if _, _, err := Project(p, FactSnapshot{}); err == nil {
		t.Fatal("inactive invalid payload accepted")
	}
	p = guardedProgram()
	p.Resources[0].DependsOn = []string{"route"}
	if _, _, err := Project(p, FactSnapshot{}); err == nil {
		t.Fatal("inactive graph cycle accepted")
	}
}

func TestSnapshotImmutableAcrossRevocationAndExpiry(t *testing.T) {
	scope := FactScope{BootID: "boot", ConfigDigest: "digest", OperatorRevision: 1, ProjectionGeneration: 1}
	store, err := NewFactStore(scope)
	if err != nil {
		t.Fatal(err)
	}
	epoch := ProducerEpoch{Epoch: 1, WorkerGeneration: 1}
	if err := store.ActivateProducer(scope, "worker", epoch); err != nil {
		t.Fatal(err)
	}
	for i, key := range []string{"health", "address"} {
		if err := store.Publish(ProtocolFact{Key: key, Producer: "worker", Scope: scope, Epoch: epoch, Sequence: uint64(i + 1), ObservedNanos: 1, ExpiresNanos: 10, Value: true}, 2); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.Snapshot(9)
	before, _, err := Project(guardedProgram(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	expired, states, err := Project(guardedProgram(), store.Snapshot(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired.Native.Network.Routes) != 0 || states["wan"].When != Indeterminate {
		t.Fatal("expiry authorized work")
	}
	if err := store.RevokeProducer(scope, "worker", epoch); err != nil {
		t.Fatal(err)
	}
	after, _, err := Project(guardedProgram(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("snapshot changed after revocation")
	}
	if store.Snapshot(9).Value("health") != Indeterminate {
		t.Fatal("revoked fact survived")
	}
}
