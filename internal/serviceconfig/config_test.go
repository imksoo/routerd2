package serviceconfig

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/ra"
)

func TestDelegatedServiceCollisionRejectedBeforeLeaseAcquisition(t *testing.T) {
	pd := Service{Name: "pd", DHCP6: &DHCP6{RequestPrefix: true, Interface: "wan0", IAID: 1, Delegations: []Delegation{{Name: "lan", Interface: "lan0", Advertise: true}}}}
	advertisement := Service{Name: "pd-lan-ra", RA: &ra.Config{Interface: "lan1", RouterLifetimeSeconds: 1800}}
	for _, services := range [][]Service{{pd, advertisement}, {advertisement, pd}} {
		if err := Validate(services); err == nil || !strings.Contains(err.Error(), "already configured") {
			t.Fatalf("generated name collision escaped preflight: %v", err)
		}
	}
	pd.DHCP6.Delegations[0].Advertise = false
	if err := Validate([]Service{pd, advertisement}); err != nil {
		t.Fatalf("address-only delegation incorrectly reserves an RA service: %v", err)
	}
}
