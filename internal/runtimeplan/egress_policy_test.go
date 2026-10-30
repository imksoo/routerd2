package runtimeplan

import (
	"testing"

	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestInactiveEgressKeepsRoutingProhibition(t *testing.T) {
	plan, err := resourceprogram.Compile(resourceprogram.Program{SchemaVersion: resourceprogram.SchemaVersion, Resources: []resourceprogram.Resource{{
		ID: "wan-policy", EgressPolicy: &resourceprogram.EgressPolicy{Groups: []resourceprogram.EgressGroup{{Name: "wan", Targets: []resourceprogram.EgressTarget{{Name: "path", Interface: "wan0", Table: 110, Priority: 10110, Mark: 272}}}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var r Runtime
	p := projection{resources: map[string]resourceprogram.ResourceState{"wan-policy": {Active: false}}}
	r.projectEgressPoliciesLocked(configuration{plan: plan}, resourceprogram.FactSnapshot{}, &p)
	if len(p.network.Routes) != 1 || !p.network.Routes[0].Blackhole || p.network.Routes[0].BootID != "" || p.network.Routes[0].ExpiresNanos != 0 {
		t.Fatalf("inactive policy lost its permanent blackhole: %+v", p.network.Routes)
	}
	if len(p.network.PolicyRules) != 2 || p.network.PolicyRules[0].Authority != (lifetime.Deadline{}) {
		t.Fatalf("inactive policy lost its permanent mark lookup: %+v", p.network.PolicyRules)
	}
	if p.network.Firewall == nil || len(p.network.Firewall.FlowMarks) != 1 || p.network.Firewall.FlowMarks[0] != 272 || len(p.network.Firewall.FlowRoutes) != 1 || len(p.network.Firewall.FlowRoutes[0].Buckets) != 0 {
		t.Fatalf("inactive policy must restore old marks without selecting new flows: %+v", p.network.Firewall)
	}
}
