package derived

import (
	"testing"

	"github.com/imksoo/routerd/pkg/api"
)

func TestReferencesPreserveNativeResourceIDsAndOwnTheirResults(t *testing.T) {
	p, err := Compile("derived", api.DerivedEventSpec{Topic: "routerd.output", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/service/wan.status.edge.status.phase}", Equals: "Healthy"}}})
	if err != nil {
		t.Fatal(err)
	}
	refs := p.References()
	if len(refs) != 1 || refs[0].Resource != "service/wan.status.edge" || refs[0].Field != "phase" || refs[0].Kind != "HealthCheck" {
		t.Fatal(refs)
	}
	refs[0].Field = "changed"
	if p.References()[0].Field != "phase" {
		t.Fatal("caller mutated compiled references")
	}
}
