package vrrp

import (
	"net/netip"
	"testing"
	"time"
)

func TestGracefulElectionDoesNotRequestVIPBeforeAdmission(t *testing.T) {
	c := Config{Resource: "ha", Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AddressOwner: true, Addresses: []string{"192.0.2.254/24"}, GracefulActivation: &GracefulActivation{Timeout: "1s"}}
	c.GracefulActivation.ReadyWhen.Fact = "service.wan.active"
	m := newMachine(c, netip.MustParseAddr("192.0.2.2"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	action := m.start(time.Unix(100, 0))
	snapshot := m.snapshot()
	if snapshot.State != StateActive || snapshot.Active || !action.advertise {
		t.Fatal("graceful election must retain MASTER advertisements while withholding VIP ownership", snapshot, action)
	}
}
