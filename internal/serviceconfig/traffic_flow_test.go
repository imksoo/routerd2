package serviceconfig

import (
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
	"testing"
)

func TestTrafficFlowPacketGroupHasOneOwner(t *testing.T) {
	services := []Service{{Name: "flows", TrafficFlow: &trafficflowconfig.Config{PacketGroup: 2}}, {Name: "denied", FirewallLog: &firewalllogconfig.Config{Group: 2}}}
	if Validate(services) == nil {
		t.Fatal("two NFLOG readers accepted for one group")
	}
	services[1].FirewallLog.Group = 3
	if err := Validate(services); err != nil {
		t.Fatal(err)
	}
}
