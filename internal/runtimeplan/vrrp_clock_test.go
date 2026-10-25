package runtimeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPWallClockAloneCannotAuthorizeOwnership(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, n, _ := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := vrrp.Snapshot{Resource: c.Resource, Interface: c.Interface, VIPInterface: c.VIPInterface, Family: c.Family, VirtualRouterID: c.VirtualRouterID, Priority: c.Priority, AcceptMode: true, Addresses: c.Addresses, VirtualMAC: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), Epoch: strings.Repeat("a", 32), Transition: 1, Active: true, State: vrrp.StateActive, ValidUntil: time.Now().Add(3 * time.Second)}
	if err := r.Accept(context.Background(), update(t, r, "service.ha", 1, s)); err == nil {
		t.Fatal("UTC-only VRRP grant acquired VIP/VMAC ownership without boot-scoped evidence")
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("rejected observation changed the network")
	}
}
