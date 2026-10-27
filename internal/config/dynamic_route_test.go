package config

import (
	"testing"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestGuardedRouteUsesProtocolLinkWithoutOwningIt(t *testing.T) {
	source := Document{SchemaVersion: SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "owner", Service: &serviceconfig.Service{Name: "wan", VRRP: &vrrp.Config{Interface: "eth0", VIPInterface: "ha4", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/24"}}}},
		{ID: "dns-route", DependsOn: []string{"owner"}, When: &resourceprogram.FactCondition{Fact: "service.wan.active"}, Route: &network.Route{Destination: "203.0.113.53/32", Interface: "ha4"}},
	}}
	plan, err := Compile(source)
	if err != nil {
		t.Fatal("route with active owner rejected:", err)
	}
	for _, iface := range plan.Native.Network.Interfaces {
		if iface.Name == "ha4" {
			t.Fatal("route adopted its protocol-owned link")
		}
	}
	source.Resources[1].When = nil
	if _, err := Compile(source); err == nil {
		t.Fatal("route without protocol lifetime accepted")
	}
}
