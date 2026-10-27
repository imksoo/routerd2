package serviceconfig

import "testing"

func dsliteService() Service {
	return Service{Name: "b4", DSLite: &DSLite{Interface: "wan0", Tunnel: "ds0", LocalAddress: "2001:db8::2", Candidates: []string{"2001:db8::1"}}}
}

func TestDSLiteExplicitPeerAndGatewayValidation(t *testing.T) {
	no := false
	tests := []struct {
		name   string
		change func(*DSLite)
		valid  bool
	}{
		{"unchanged-default", func(c *DSLite) {}, true},
		{"legacy-address-prefix-without-peer", func(c *DSLite) { c.B4Address = "198.51.100.2/24" }, true},
		{"peer-only", func(c *DSLite) { c.B4Peer = "192.0.0.1/32" }, true},
		{"peer-without-default-route", func(c *DSLite) { c.B4Peer, c.DefaultRoute = "192.0.0.1/32", &no }, true},
		{"peer-and-gateway", func(c *DSLite) { c.B4Peer, c.RouteGateway = "192.0.0.1/32", "192.0.0.1" }, true},
		{"link-local-inner", func(c *DSLite) {
			c.B4Address, c.B4Peer, c.RouteGateway = "169.254.42.2/32", "169.254.42.1/32", "169.254.42.1"
		}, true},
		{"peer-v6", func(c *DSLite) { c.B4Peer = "2001:db8::1/128" }, false},
		{"peer-mapped", func(c *DSLite) { c.B4Peer = "::ffff:192.0.0.1/128" }, false},
		{"peer-prefix", func(c *DSLite) { c.B4Peer = "192.0.0.1/24" }, false},
		{"peer-no-prefix", func(c *DSLite) { c.B4Peer = "192.0.0.1" }, false},
		{"peer-noncanonical", func(c *DSLite) { c.B4Peer = "192.0.00.1/32" }, false},
		{"peer-default-local", func(c *DSLite) { c.B4Peer = "192.0.0.2/32" }, false},
		{"peer-explicit-local", func(c *DSLite) { c.B4Address, c.B4Peer = "198.51.100.1/32", "198.51.100.1/32" }, false},
		{"local-prefix-with-peer", func(c *DSLite) { c.B4Address, c.B4Peer = "192.0.0.2/24", "192.0.0.1/32" }, false},
		{"peer-unspecified", func(c *DSLite) { c.B4Peer = "0.0.0.0/32" }, false},
		{"peer-multicast", func(c *DSLite) { c.B4Peer = "224.0.0.1/32" }, false},
		{"peer-broadcast", func(c *DSLite) { c.B4Peer = "255.255.255.255/32" }, false},
		{"gateway-without-peer", func(c *DSLite) { c.RouteGateway = "192.0.0.1" }, false},
		{"gateway-different-peer", func(c *DSLite) { c.B4Peer, c.RouteGateway = "192.0.0.1/32", "192.0.0.9" }, false},
		{"gateway-v6", func(c *DSLite) { c.B4Peer, c.RouteGateway = "192.0.0.1/32", "2001:db8::1" }, false},
		{"gateway-prefix", func(c *DSLite) { c.B4Peer, c.RouteGateway = "192.0.0.1/32", "192.0.0.1/32" }, false},
		{"gateway-noncanonical", func(c *DSLite) { c.B4Peer, c.RouteGateway = "192.0.0.1/32", "192.0.00.1" }, false},
		{"gateway-disabled-route", func(c *DSLite) { c.B4Peer, c.RouteGateway, c.DefaultRoute = "192.0.0.1/32", "192.0.0.1", &no }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := dsliteService()
			test.change(s.DSLite)
			if err := Validate([]Service{s}); (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}
