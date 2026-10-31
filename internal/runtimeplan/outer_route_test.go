package runtimeplan

import (
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

func TestOuterIPv4RouteUsesDHCPGatewayForOfflinkEndpoint(t *testing.T) {
	clock := boottime.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(time.Second)}
	until := lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds + int64(time.Minute))}
	route, ok := outerIPv4Route(network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, "203.0.113.2", "wan0", &leasedIPv4{gateway: netip.MustParseAddr("192.0.2.254")}, clock, until)
	if !ok || route.Destination != "203.0.113.2/32" || route.Interface != "wan0" || route.Gateway != "192.0.2.254" || route.BootID != until.BootID || route.ExpiresNanos != until.Nanos {
		t.Fatalf("off-link RR endpoint was not pinned through its DHCP gateway: %+v", route)
	}
}
