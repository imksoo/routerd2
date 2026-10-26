package network

import "testing"

func TestRoutingDomainsFollowDeclaredMastersAndIncludeTheVRFDevice(t *testing.T) {
	blue, bridge := "blue", "bridge0"
	d := Desired{Links: []Link{{Name: blue, Kind: "vrf", VRFTable: 1001}, {Name: bridge, Kind: "bridge"}}, Interfaces: []Interface{
		{Name: "port0", Master: &bridge},
		{Name: bridge, Master: &blue},
		{Name: "wan0"},
		{Name: blue},
	}}
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	domains := RoutingDomains(d)
	for _, name := range []string{"port0", bridge, blue} {
		if domain := domains[name]; domain.VRF != blue || domain.Table != 1001 {
			t.Fatal("interface lost its routing domain", name, domain)
		}
	}
	if domain := domains["wan0"]; domain.VRF != "" || domain.Table != 254 {
		t.Fatal("unbound interface borrowed the VRF", domain)
	}
}
