package resourceprogram

import (
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPTrackReferencesBindAllDeclarationsWithoutGatingTheVIP(t *testing.T) {
	cfg := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Resource: "input", MaxAgeMillis: 1500}}}
	for _, source := range []serviceconfig.Service{
		{Name: "input", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}},
		{Name: "input", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}},
		{Name: "input", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002}}}}},
	} {
		t.Run(source.Role(), func(t *testing.T) {
			program := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
				{ID: "vip", Service: &serviceconfig.Service{Name: "ha", VRRP: &cfg}},
				{ID: "input", When: &FactCondition{Fact: "service.switch.active"}, Service: &source},
			}}
			plan, err := Compile(program)
			if err != nil {
				t.Fatal(err)
			}
			projected, _ := plan.Project(FactSnapshot{})
			if len(projected.Services) != 1 || projected.Services[0].Name != "ha" {
				t.Fatal("inactive track stopped its election", projected.Services)
			}
			if err := serviceconfig.Validate(projected.Services); err != nil {
				t.Fatal("soft reference required inactive provider in projection", err)
			}
			program.Resources = program.Resources[:1]
			if _, err := Compile(program); err == nil {
				t.Fatal("missing declaration accepted")
			}
			program.Resources = append(program.Resources, Resource{ID: "input", Service: &serviceconfig.Service{Name: "input", RA: &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: "wan0", RouterLifetimeSeconds: 1800}}}})
			if _, err := Compile(program); err == nil {
				t.Fatal("unobserved provider accepted as a health source")
			}
		})
	}
}
