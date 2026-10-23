package dhcpserverconfig

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

func validConfig() Config {
	return Normalize(Config{Resource: "DHCPv4Server/lan", Interface: "lan0", ServerAddress: "192.0.2.1", StatePath: "/var/lib/routerd/dhcp/leases.json", Primary: Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.200", Router: "192.0.2.1", DNSServers: []string{"192.0.2.1"}, Domain: "lan.example", LeaseSeconds: 120}})
}

func TestConfigRejectsAmbiguousOrUnsupportedIntent(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"empty-identity", func(c *Config) { c.Resource = "" }},
		{"bad-interface", func(c *Config) { c.Interface = "../lan" }},
		{"vrf-interface", func(c *Config) { c.VRF = c.Interface }},
		{"v6-server", func(c *Config) { c.ServerAddress = "2001:db8::1" }},
		{"host-prefix", func(c *Config) { c.Primary.Prefix = "192.0.2.1/24" }},
		{"reverse-pool", func(c *Config) { c.Primary.Start = "192.0.2.201" }},
		{"network-pool", func(c *Config) { c.Primary.Start = "192.0.2.0" }},
		{"broadcast-pool", func(c *Config) { c.Primary.End = "192.0.2.255" }},
		{"offlink-router", func(c *Config) { c.Primary.Router = "198.51.100.1" }},
		{"short-lease", func(c *Config) { c.Primary.LeaseSeconds = 1 }},
		{"snapshot-bound", func(c *Config) { c.MaxLeases = 2049 }},
		{"negative-probe", func(c *Config) { c.ProbeTimeoutMilliseconds = -1 }},
		{"state-path", func(c *Config) { c.StatePath = "leases.json" }},
		{"path-overlap", func(c *Config) { c.StatusPath = c.StatePath + ".lock" }},
		{"scope-selector", func(c *Config) { s := c.Primary; s.Name = "secondary"; c.Scopes = []Scope{s} }},
		{"option-width", func(c *Config) { c.Primary.Options = []Option{{Code: 65535, Value: "1"}} }},
		{"protocol-option", func(c *Config) { c.Primary.Options = []Option{{Code: 54, Value: "192.0.2.2"}} }},
		{"backend-text", func(c *Config) { c.Primary.Options = []Option{{Code: 200, Value: "unknown,text"}} }},
		{"reservation-gateway", func(c *Config) {
			c.Reservations = []Reservation{{MACAddress: "02:00:00:00:00:01", IPAddress: "192.0.2.1"}}
		}},
		{"reservation-offlink", func(c *Config) {
			c.Reservations = []Reservation{{MACAddress: "02:00:00:00:00:01", IPAddress: "198.51.100.5"}}
		}},
		{"multicast-mac", func(c *Config) {
			c.Reservations = []Reservation{{MACAddress: "01:00:00:00:00:01", IPAddress: "192.0.2.50"}}
		}},
		{"duplicate-relay", func(c *Config) { c.TrustedRelays = []string{"192.0.2.2", "192.0.2.2"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.edit(&c)
			if err := Validate(c); err == nil {
				t.Fatal("invalid intent accepted")
			}
		})
	}
	c := validConfig()
	c.Reservations = []Reservation{{MACAddress: "02:00:00:00:00:01", IPAddress: "192.0.2.50", Hostname: "printer"}}
	if err := Validate(c); err != nil {
		t.Fatalf("reservation outside dynamic pool: %v", err)
	}
}

func TestScopeSelectorsAndNormalizationAreStable(t *testing.T) {
	c := validConfig()
	oui := c.Primary
	oui.Name = "oui"
	oui.Match = Match{OUIPrefixes: []string{"02:aa:bb"}}
	exact := c.Primary
	exact.Name = "exact"
	exact.Match = Match{MACAddresses: []string{"02:AA:BB:00:00:01"}}
	c.Scopes = []Scope{oui, exact}
	c.Reservations = []Reservation{{MACAddress: "02:AA:BB:00:00:01", IPAddress: "192.0.2.60", Scope: "exact"}}
	before, _ := json.Marshal(c)
	normalized := Normalize(c)
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("caller intent mutated")
	}
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	mac, _ := net.ParseMAC("02:aa:bb:00:00:01")
	if got := SelectScope(normalized, mac).Name; got != "exact" {
		t.Fatal(got)
	}
	mac, _ = net.ParseMAC("02:aa:bb:00:00:02")
	if got := SelectScope(normalized, mac).Name; got != "oui" {
		t.Fatal(got)
	}
	normalized.Scopes[0].Match.OUIPrefixes[0] = "02:cc:dd"
	if c.Scopes[0].Match.OUIPrefixes[0] != "02:aa:bb" {
		t.Fatal("slices alias caller")
	}
	c.Scopes[1].Match.OUIPrefixes = []string{"02-aa-bb"}
	if err := Validate(c); err == nil {
		t.Fatal("duplicate OUI selectors accepted")
	}
}

func TestLegacyAdapterPreservesDefaultsProfilesAndOptions(t *testing.T) {
	seed := validConfig()
	spec := api.DHCPv4ServerSpec{Interface: "lan0", AddressPool: api.DHCPAddressPoolSpec{Start: "192.0.2.100", End: "192.0.2.150", LeaseTime: "2h"}, Gateway: "192.0.2.1", DNSServers: []string{"192.0.2.53"}, Domain: "example.test", Profiles: []api.DHCPv4ServerProfileSpec{{Name: "voice", AddressPool: api.DHCPAddressPoolSpec{Start: "192.0.2.160", End: "192.0.2.180", LeaseTime: "1d"}, Netmask: "255.255.255.0", Gateway: "192.0.2.1", Options: []Option{{Name: "mtu", Value: "1400"}}}}, Scopes: []api.DHCPv4ServerScopeSpec{{Name: "phones", ProfileRef: "voice", Match: Match{OUIPrefixes: []string{"02:aa:bb"}}}}}
	c, err := FromSpec(seed, spec, []api.DHCPv4ReservationSpec{{MACAddress: "02:00:00:00:00:01", IPAddress: "192.0.2.10", Hostname: "printer", LeaseTime: "30m"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Primary.LeaseSeconds != 7200 || c.Primary.Router != "192.0.2.1" || !reflect.DeepEqual(c.Primary.DNSServers, []string{"192.0.2.53"}) || c.Scopes[0].LeaseSeconds != 86400 || c.Scopes[0].Options[0].Name != "mtu" || c.Reservations[0].LeaseSeconds != 1800 {
		t.Fatalf("%+v", c)
	}
	spec.RouterSource = "none"
	spec.DNSSource = "none"
	c, err = FromSpec(seed, spec, nil)
	if err != nil || c.Primary.Router != "" || len(c.Primary.DNSServers) != 0 {
		t.Fatalf("none sources: %+v %v", c, err)
	}
	spec.RouterSource = ""
	spec.DNSSource = ""
	spec.Authoritative = true
	c, err = FromSpec(seed, spec, nil)
	if err != nil || c.Primary.Router != seed.ServerAddress || !reflect.DeepEqual(c.Primary.DNSServers, []string{seed.ServerAddress}) {
		t.Fatalf("scope-style defaults: %+v %v", c, err)
	}
	for _, edit := range []func(*api.DHCPv4ServerSpec){
		func(s *api.DHCPv4ServerSpec) { s.DNS.Enabled = true },
		func(s *api.DHCPv4ServerSpec) { s.LeaseFile = "/var/lib/misc/dnsmasq.leases" },
		func(s *api.DHCPv4ServerSpec) { s.GatewayFrom.Resource = "VirtualAddress/router" },
		func(s *api.DHCPv4ServerSpec) { s.Server = "kea" },
		func(s *api.DHCPv4ServerSpec) { s.Role = "transit" },
		func(s *api.DHCPv4ServerSpec) { s.Scopes[0].Gateway = "192.0.2.1" },
	} {
		b, _ := json.Marshal(spec)
		var s api.DHCPv4ServerSpec
		_ = json.Unmarshal(b, &s)
		edit(&s)
		if _, err := FromSpec(seed, s, nil); err == nil {
			t.Fatal("unresolved/backend intent silently accepted")
		}
	}
}

func TestOptionEncodingHasExactWireShapes(t *testing.T) {
	out, err := EncodeOptions([]Option{{Name: "router", Value: "192.0.2.1,192.0.2.2"}, {Name: "mtu", Value: "1450"}, {Name: "domain-search", Value: "lan.example,corp.example"}, {Name: "classless-static-route", Value: "0.0.0.0/0,192.0.2.1,198.51.100.0/24,192.0.2.2"}, {Code: 224, Value: "hex:01:02:ff"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out[3], []byte{192, 0, 2, 1, 192, 0, 2, 2}) || !reflect.DeepEqual(out[26], []byte{5, 170}) || !reflect.DeepEqual(out[121], []byte{0, 192, 0, 2, 1, 24, 198, 51, 100, 192, 0, 2, 2}) || !reflect.DeepEqual(out[224], []byte{1, 2, 255}) {
		t.Fatalf("%#v", out)
	}
	if !strings.Contains(string(out[119]), "\x03lan\x07example\x00") {
		t.Fatal(out[119])
	}
	if _, err := EncodeOptions([]Option{{Code: 26, Name: "router", Value: "192.0.2.1"}}); err == nil {
		t.Fatal("name/code mismatch")
	}
}

func TestSnapshotBoundaryRejectsStaleOrUnownedClaims(t *testing.T) {
	c := validConfig()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s := Snapshot{Resource: c.Resource, Interface: c.Interface, ServerAddress: c.ServerAddress, Epoch: strings.Repeat("a", 32), Sequence: 1, Active: true, ObservedAt: now, ValidUntil: now.Add(5 * time.Second), Leases: []Lease{{MACAddress: "02:00:00:00:00:01", IPAddress: "192.0.2.100", Hostname: "host", Scope: "default", AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(120 * time.Second)}}}
	if err := ValidateSnapshot(c, s); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Snapshot){
		func(s *Snapshot) { s.Interface = "wan0" }, func(s *Snapshot) { s.Sequence = 0 }, func(s *Snapshot) { s.Epoch = "bad" },
		func(s *Snapshot) { s.ValidUntil = s.ObservedAt.Add(6 * time.Second) },
		func(s *Snapshot) { s.Leases[0].ExpiresAt = now }, func(s *Snapshot) { s.Leases[0].ExpiresAt = now.Add(time.Hour) },
		func(s *Snapshot) { s.Leases[0].IPAddress = "192.0.2.1" }, func(s *Snapshot) { s.Leases[0].Scope = "unknown" },
		func(s *Snapshot) { s.Leases[0].ClientID = "x" }, func(s *Snapshot) { s.Leases[0].Reserved = true },
		func(s *Snapshot) { s.Leases = append(s.Leases, s.Leases[0]) }, func(s *Snapshot) { s.Active = false },
	} {
		b, _ := json.Marshal(s)
		var candidate Snapshot
		_ = json.Unmarshal(b, &candidate)
		edit(&candidate)
		if err := ValidateSnapshot(c, candidate); err == nil {
			t.Fatalf("bad snapshot accepted: %+v", candidate)
		}
	}
	s.Active = false
	s.Leases = nil
	s.ValidUntil = time.Time{}
	if err := ValidateSnapshot(c, s); err != nil {
		t.Fatal(err)
	}
}
