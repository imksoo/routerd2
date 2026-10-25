package serviceconfig

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

func TestVRRPTracksRequireConfiguredHealthSources(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Service: "probe", MaxAgeMillis: 1500}}}
	ha := Service{Name: "ha", VRRP: &c}
	probe := Service{Name: "probe", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}
	if err := Validate([]Service{ha, probe}); err != nil {
		t.Fatal(err)
	}
	for _, sources := range [][]Service{
		{ha},
		{ha, {Name: "probe", RA: &ra.Config{Interface: "lan0", RouterLifetimeSeconds: 1800}}},
	} {
		if err := Validate(sources); err == nil || !strings.Contains(err.Error(), "not a configured Health service") {
			t.Fatal("invalid VRRP tracking reference accepted", err)
		}
	}
}
