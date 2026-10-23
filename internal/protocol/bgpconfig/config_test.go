package bgpconfig

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
)

func validConfig() Config {
	return Config{Resource: "BGPRouter/test", SocketPath: "/run/routerd/bgp/control.sock", StatePath: "/var/lib/routerd/bgp/applied.json", Runtime: bgpdaemon.AppliedConfig{Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpdaemon.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 64513}}}}
}

func TestConfigValidationRejectsUnimplementedAndAmbiguousIntent(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"asn", func(c *Config) { c.Runtime.Global.ASN = 0 }},
		{"router-id", func(c *Config) { c.Runtime.Global.RouterID = "::1" }},
		{"port", func(c *Config) { c.Runtime.Global.ListenPort = 65536 }},
		{"family", func(c *Config) { c.Runtime.Global.Families = []string{"l2vpn-evpn"} }},
		{"socket-list-injection", func(c *Config) { c.GRPCSocketPath = "/tmp/x,0.0.0.0:50051" }},
		{"lock-collision", func(c *Config) { c.StatusPath = c.StatePath + ".lock" }},
		{"bfd-reference", func(c *Config) { p := c.Runtime.Peers["192.0.2.2"]; p.BFD = "fast"; c.Runtime.Peers["192.0.2.2"] = p }},
		{"multihop", func(c *Config) {
			p := c.Runtime.Peers["192.0.2.2"]
			p.EbgpMultihop = 256
			c.Runtime.Peers["192.0.2.2"] = p
		}},
		{"reflector", func(c *Config) {
			p := c.Runtime.Peers["192.0.2.2"]
			p.RouteReflectorClient = true
			c.Runtime.Peers["192.0.2.2"] = p
		}},
		{"discarded-empty-path", func(c *Config) { c.Runtime.Paths = []bgpdaemon.AppliedPath{{Prefix: "10.0.0.0/24"}} }},
		{"community-overflow", func(c *Config) {
			c.Runtime.Paths = []bgpdaemon.AppliedPath{{Source: "routerd-static", Prefix: "10.0.0.0/24", Attrs: bgpdaemon.AppliedPathAttrs{Communities: []string{"65536:1"}}}}
		}},
		{"next-hop-family", func(c *Config) {
			c.Runtime.Paths = []bgpdaemon.AppliedPath{{Source: "routerd-static", Prefix: "10.0.0.0/24", Attrs: bgpdaemon.AppliedPathAttrs{NextHop: "2001:db8::1"}}}
		}},
		{"prefix-range", func(c *Config) {
			c.Runtime.Global.ImportPolicy = bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"192.0.2.0/24"}, AllowedPrefixLengthMax: 23}
		}},
		{"no-transport-peer", func(c *Config) { c.PeerTransports = map[string]PeerTransport{"192.0.2.3": {RemotePort: 179}} }},
		{"bad-fib-interface", func(c *Config) { c.FIBInterface = "../eth0" }},
		{"local-table", func(c *Config) { c.Table = 255 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.edit(&cfg)
			if err := Validate(cfg); err == nil {
				t.Fatal("invalid intent accepted")
			}
		})
	}
}

func TestNormalizeDoesNotMutateCallerAndHasExplicitDefaults(t *testing.T) {
	c := validConfig()
	c.Runtime.Global.Families = []string{"ipv6-unicast", "ipv4-unicast"}
	before, _ := json.Marshal(c)
	n := Normalize(c)
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("normalization modified caller input")
	}
	if n.Table != 254 || n.RouteMetric != 200 || n.EffectTimeoutMilliseconds != 2000 {
		t.Fatalf("defaults: %#v", n)
	}
	if !reflect.DeepEqual(n.Runtime.Global.Families, []string{"ipv4-unicast", "ipv6-unicast"}) {
		t.Fatal(n.Runtime.Global.Families)
	}
	p := n.Runtime.Peers["192.0.2.2"]
	p.Password = "changed"
	n.Runtime.Peers["192.0.2.2"] = p
	if c.Runtime.Peers["192.0.2.2"].Password != "" {
		t.Fatal("peer map shares mutable storage")
	}
}
