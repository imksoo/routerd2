package config

import (
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPDynamicInterfaceRequiresDependencyAndActiveGuard(t *testing.T) {
	active := resourceprogram.FactCondition{Fact: "service.ha.active"}
	for _, mode := range []string{"guarded", "missing-edge", "missing-guard", "negative-guard", "partial-any", "all-guard", "all-any-guard", "owner-readiness"} {
		t.Run(mode, func(t *testing.T) {
			source := Document{SchemaVersion: SchemaVersion, Resources: []resourceprogram.Resource{
				{ID: "ha-owner", Service: &serviceconfig.Service{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", VIPInterface: "ha4", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/24"}}}},
				{ID: "client", DependsOn: []string{"ha-owner"}, When: &active, Service: &serviceconfig.Service{Name: "dependent", DHCP4: &serviceconfig.DHCP4{Interface: "ha4"}}},
			}}
			consumer := &source.Resources[1]
			switch mode {
			case "missing-edge":
				consumer.DependsOn = nil
			case "missing-guard":
				consumer.When = nil
			case "negative-guard":
				consumer.When = &resourceprogram.FactCondition{Not: &active}
			case "partial-any":
				consumer.When = &resourceprogram.FactCondition{Any: []resourceprogram.FactCondition{active, {Fact: "service.ha.ready"}}}
			case "all-guard":
				consumer.When = &resourceprogram.FactCondition{All: []resourceprogram.FactCondition{active, {Fact: "service.ha.ready"}}}
			case "all-any-guard":
				consumer.When = &resourceprogram.FactCondition{Any: []resourceprogram.FactCondition{active, {All: []resourceprogram.FactCondition{active, {Fact: "service.ha.ready"}}}}}
			case "owner-readiness":
				consumer.When = nil
				source.Resources[0].ReadyWhen = &active
			}
			_, err := Compile(source)
			valid := mode == "guarded" || mode == "all-guard" || mode == "all-any-guard" || mode == "owner-readiness"
			if valid && err != nil {
				t.Fatal("explicitly guarded VRRP dependent rejected:", err)
			}
			if !valid && err == nil {
				t.Fatal("unprotected dynamic interface dependency accepted")
			}
		})
	}
}
