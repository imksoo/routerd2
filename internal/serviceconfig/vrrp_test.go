package serviceconfig

import (
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

func TestVRRPTrackDoesNotRequireInactiveProviderInProjectedServices(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Resource: "service/probe", MaxAgeMillis: 1500}}}
	ha := Service{Name: "ha", VRRP: &c}
	probe := Service{Name: "probe", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}
	if err := Validate([]Service{ha, probe}); err != nil {
		t.Fatal(err)
	}
	// Cross-resource references belong to the full source compiler. A
	// projected conditional provider can disappear while its VIP continues.
	if err := Validate([]Service{ha}); err != nil {
		t.Fatal(err)
	}
}
