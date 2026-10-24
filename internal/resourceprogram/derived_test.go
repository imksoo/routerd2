package resourceprogram

import (
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestDerivedEventValidatesTypedReferencesBeforeProjection(t *testing.T) {
	spec := api.DerivedEventSpec{Topic: "routerd.wan", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/service/wan.status.phase}", Equals: "Healthy"}}, Hysteresis: "1s"}
	source := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "service/wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1", Protocol: "tcp", Port: 80}}},
		{ID: "derived", DerivedEvent: &spec},
	}}
	p, err := Compile(source)
	if err != nil || len(p.Native.DerivedEvents) != 1 {
		t.Fatal(p, err)
	}
	if len(p.Native.DerivedEvents[0].References) != 1 || p.Native.DerivedEvents[0].References[0].Resource != "service/wan" {
		t.Fatal("status references did not bind the stable resource ID", p.Native.DerivedEvents)
	}
	source.Resources[0].When = &FactCondition{Fact: "service.wan.ready"}
	projected, _, err := Project(source, FactSnapshot{})
	if err != nil || len(projected.Native.DerivedEvents) != 1 || len(projected.Native.Services) != 0 {
		t.Fatal("inactive provider became an invalid configuration reference", projected, err)
	}
	for _, field := range []string{"${HealthCheck/missing.status.phase}", "${HealthCheck/service/wan.status.unknown}", "${Interface/service/wan.status.phase}"} {
		spec.Inputs[0].Field = field
		if _, err = Compile(source); err == nil {
			t.Fatal("invalid reference accepted", field)
		}
	}
}

func TestDerivedBFDReferencesValidateAgainstAllDeclaredTypedProviders(t *testing.T) {
	for _, field := range []string{"phase", "peerStates", "peers", "observedAt"} {
		spec := api.DerivedEventSpec{Topic: "routerd.link", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/bfd/source.status." + field + "}", NotEmpty: true}}}
		source := Program{SchemaVersion: SchemaVersion, Resources: []Resource{{ID: "bfd/source", When: &FactCondition{Fact: "service.gate.ready"}, Service: &serviceconfig.Service{Name: "edge", BFD: &bfd.Config{Interface: "eth0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}}}, {ID: "derived", DerivedEvent: &spec}}}
		p, err := Compile(source)
		if err != nil || len(p.Native.DerivedEvents) != 1 {
			t.Fatal(field, p, err)
		}
		projected, _, err := Project(source, FactSnapshot{})
		if err != nil || len(projected.Native.Services) != 0 || len(projected.Native.DerivedEvents) != 1 {
			t.Fatal("inactive provider reference was discarded", field, err)
		}
		for _, bad := range []string{"${BFD/missing.status.phase}", "${HealthCheck/bfd/source.status.phase}", "${BFD/bfd/source.status.unsupported}", "${BFD/bfd/source.status.peers.state}"} {
			spec.Inputs[0].Field = bad
			if _, err = Compile(source); err == nil {
				t.Fatal("invalid BFD reference accepted", bad)
			}
		}
	}
}
