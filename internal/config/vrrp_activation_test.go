package config

import (
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"testing"
)

func TestGracefulVRRPAdmissionRejectsOwnUnpublishedActiveDependency(t *testing.T) {
	for _, fact := range []string{"service.ha.active", "service.ha.elected", "service.wan.active"} {
		c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Addresses: []string{"192.0.2.254/24"}, GracefulActivation: &vrrp.GracefulActivation{ReadyWhen: resourceprogram.FactCondition{Fact: fact}}}
		_, err := Compile(FromNative(Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}))
		if fact == "service.ha.active" && err == nil {
			t.Fatal("publication depended on its own unpublished active fact")
		}
		if fact != "service.ha.active" && err != nil {
			t.Fatal("non-circular admission rejected", fact, err)
		}
	}
}
