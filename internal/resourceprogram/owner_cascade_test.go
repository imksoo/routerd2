package resourceprogram

import (
	"reflect"
	"testing"
)

func TestRemoveOwnedTransitiveAndStable(t *testing.T) {
	src := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "independent", Interface: nil},
		{ID: "child", Owner: "root"},
		{ID: "grandchild", Owner: "child"},
		{ID: "root"},
	}}
	got, ids, err := RemoveOwned(src, "root")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"child", "grandchild", "root"}) {
		t.Fatalf("removed %v", ids)
	}
	if len(got.Resources) != 1 || got.Resources[0].ID != "independent" {
		t.Fatalf("survivors: %+v", got)
	}
	if len(src.Resources) != 4 {
		t.Fatal("mutated source")
	}
}

func TestRemoveOwnedRejectsDanglingDependency(t *testing.T) {
	src := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "root"},
		{ID: "dependent", DependsOn: []string{"root"}},
	}}
	got, _, err := RemoveOwned(src, "root")
	if err == nil || len(got.Resources) != 0 {
		t.Fatalf("expected atomic failure: %v %+v", err, got)
	}
}

func TestRemoveOwnedMissing(t *testing.T) {
	_, _, err := RemoveOwned(Program{SchemaVersion: SchemaVersion}, "missing")
	if err == nil {
		t.Fatal("missing resource accepted")
	}
}
