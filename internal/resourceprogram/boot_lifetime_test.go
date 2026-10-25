package resourceprogram

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/network"
)

func TestOperatorCannotDeclareInternalBootAddressAuthority(t *testing.T) {
	for _, inactive := range []bool{false, true} {
		r := Resource{ID: "address", Address: &Address{Interface: "wan0", CIDR: "192.0.2.9/24", Options: &network.AddressOptions{BootLifetime: network.BootLifetime{BootID: "11111111-2222-4333-8444-555555555555", PreferredNanos: 10, ValidNanos: 20}}}}
		if inactive {
			r.When = &FactCondition{Fact: "service.wan.active"}
		}
		if _, err := Compile(Program{SchemaVersion: SchemaVersion, Resources: []Resource{r}}); err == nil || !strings.Contains(err.Error(), "boot lifetime") {
			t.Fatal("operator supplied internal boot projection authority", inactive)
		}
	}
}

func TestOperatorCannotDeclareInternalBootRouteAuthority(t *testing.T) {
	for _, inactive := range []bool{false, true} {
		r := Resource{ID: "route", Route: &network.Route{Destination: "203.0.113.0/24", Interface: "wan0", BootID: "11111111-2222-4333-8444-555555555555", ExpiresNanos: 10}}
		if inactive {
			r.When = &FactCondition{Fact: "service.wan.active"}
		}
		if _, err := Compile(Program{SchemaVersion: SchemaVersion, Resources: []Resource{r}}); err == nil || !strings.Contains(err.Error(), "boot deadline") {
			t.Fatal("operator supplied internal route authority", inactive, err)
		}
	}
}
