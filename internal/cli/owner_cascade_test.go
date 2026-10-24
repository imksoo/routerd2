package cli

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestCandidateDeleteOwnerCascade(t *testing.T) {
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "wan", Interface: &network.Interface{Name: "eth0"}},
		{ID: "wan-address", Owner: "wan", Address: &resourceprogram.Address{Interface: "eth0", CIDR: "192.0.2.2/24"}},
		{ID: "other", Interface: &network.Interface{Name: "eth1"}},
	}}
	c := &candidate{document: doc}
	if err := c.mutate("resources[wan]", "", true); err != nil {
		t.Fatal(err)
	}
	if len(c.document.Resources) != 1 || c.document.Resources[0].ID != "other" {
		t.Fatalf("cascade did not preserve unrelated resources: %+v", c.document.Resources)
	}
	if len(doc.Resources) != 3 {
		t.Fatal("original source mutated")
	}
}

func TestCandidateDeleteOwnerDependencyFailureIsAtomic(t *testing.T) {
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "wan", Interface: &network.Interface{Name: "eth0"}},
		{ID: "dependent", DependsOn: []string{"wan"}, Interface: &network.Interface{Name: "eth1"}},
	}}
	c := &candidate{document: doc}
	err := c.mutate("resources[wan]", "", true)
	if err == nil || !strings.Contains(err.Error(), "depends on removed") {
		t.Fatalf("expected dangling dependency rejection, got %v", err)
	}
	if len(c.document.Resources) != 2 {
		t.Fatal("failed mutation partially modified candidate")
	}
}
