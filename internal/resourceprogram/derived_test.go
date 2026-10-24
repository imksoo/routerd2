package resourceprogram

import (
	"testing"

	"github.com/imksoo/routerd/pkg/api"
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
	for _, field := range []string{"${HealthCheck/missing.status.phase}", "${HealthCheck/service/wan.status.unknown}", "${Interface/service/wan.status.phase}"} {
		spec.Inputs[0].Field = field
		if _, err = Compile(source); err == nil {
			t.Fatal("invalid reference accepted", field)
		}
	}
}
