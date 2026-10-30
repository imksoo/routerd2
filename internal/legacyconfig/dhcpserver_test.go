// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"gopkg.in/yaml.v3"
)

func mutateDHCPInput(t *testing.T, data []byte, edit func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	spec := document["spec"].(map[string]any)
	var resources []map[string]any
	for _, value := range spec["resources"].([]any) {
		resources = append(resources, value.(map[string]any))
	}
	spec["resources"] = edit(resources)
	output, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func dhcpTestSpec(t *testing.T, resources []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, r := range resources {
		if r["kind"] == kind {
			return r["spec"].(map[string]any)
		}
	}
	t.Fatal("test resource missing", kind)
	return nil
}

func TestDHCPStaticServerProfilesReservationsAndWireOptions(t *testing.T) {
	result := compiled(t, "dhcp-server")
	s := service(t, result.Document, "dhcp_server")
	c := s.DHCPServerConfig("/var/lib/routerd")
	if c.Interface != "eth1" || c.ServerAddress != "192.0.2.1" || c.Primary.Prefix != "192.0.2.0/24" || c.MaxLeases != 1000 || c.Authoritative {
		t.Fatalf("server identity, subnet or old daemon defaults changed: %+v", c)
	}
	if c.Primary.Start != "192.0.2.100" || c.Primary.End != "192.0.2.119" || c.Primary.LeaseSeconds != 7200 || c.Primary.Router != "192.0.2.254" || c.Primary.Domain != "lab.example" || !reflect.DeepEqual(c.Primary.NTPServers, []string{"192.0.2.1"}) || !reflect.DeepEqual(c.Primary.DNSServers, []string{"192.0.2.53"}) {
		t.Fatal("primary typed DHCP intent or static address source changed", c.Primary)
	}
	if len(c.Scopes) != 2 || c.Scopes[0].Name != "phones" || c.Scopes[0].LeaseSeconds != 7200 || c.Scopes[0].Domain != "" || c.Scopes[1].LeaseSeconds != 86400 || c.Scopes[1].Domain != "" || c.Scopes[1].Router != "192.0.2.1" || !reflect.DeepEqual(c.Scopes[1].DNSServers, []string{"192.0.2.1"}) {
		t.Fatal("scope lease inheritance, domain isolation or dnsmasq defaults changed", c.Scopes)
	}
	mac, _ := net.ParseMAC("02:aa:bb:00:00:01")
	if got := dhcpserverconfig.SelectScope(c, mac); got.Name != "phones" || got.Start != "192.0.2.160" {
		t.Fatal("migrated OUI selector no longer selects its profile")
	}
	if len(c.Reservations) != 1 || c.Reservations[0].IPAddress != "192.0.2.20" || c.Reservations[0].Hostname != "printer" || c.Reservations[0].LeaseSeconds != 0 {
		t.Fatal("out-of-pool reservation or inherited lease changed", c.Reservations)
	}
	if !reflect.DeepEqual(c.Primary.Options, []dhcpserverconfig.Option{{Name: "domain-search", Value: "lab.example,corp.example"}, {Code: 28, Value: "192.0.2.255"}}) {
		t.Fatal("dnsmasq options changed", c.Primary.Options)
	}
	if !reflect.DeepEqual(c.Reservations[0].Options, []dhcpserverconfig.Option{{Name: "mtu", Value: "1450"}}) {
		t.Fatal("reservation option override lost", c.Reservations[0].Options)
	}

	if len(mapping(t, result, "DHCPv4Reservation/printer").Targets) != 1 || !strings.Contains(strings.Join(mapping(t, result, "DHCPv4Server/lan-v4").Notes, " "), "not imported") {
		t.Fatal("reservation ownership and lease state boundary were not reported")
	}
}

func TestDHCPDefaultsAliasesAndUnqualifiedReservation(t *testing.T) {
	data := mutateDHCPInput(t, fixture(t, "dhcp-server"), func(rs []map[string]any) []map[string]any {
		s := dhcpTestSpec(t, rs, "DHCPv4Server")
		for _, field := range []string{"profiles", "scopes", "addressPool", "gateway", "dnsServers", "ntpServerFrom", "domain", "options"} {
			delete(s, field)
		}
		s["rangeStart"], s["rangeEnd"], s["leaseTime"] = "192.0.2.100", "192.0.2.109", "3h"
		delete(dhcpTestSpec(t, rs, "DHCPv4Reservation"), "server")
		return rs
	})
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	c := service(t, result.Document, "dhcp_server").DHCPServer
	if c.Primary.LeaseSeconds != 10800 || c.Primary.Router != "192.0.2.1" || !reflect.DeepEqual(c.Primary.DNSServers, []string{"192.0.2.1"}) || len(c.Reservations) != 1 {
		t.Fatal("range alias normalization invented scope-style option defaults", c)
	}
	if len(mapping(t, result, "DHCPv4Reservation/printer").Notes) == 0 {
		t.Fatal("unqualified reservation owner was not made reviewable")
	}
}

func TestDHCPMigrationRejectsIgnoredDynamicOrBackendFields(t *testing.T) {
	const secret = "must-not-appear-in-dhcp-errors"
	cases := []struct {
		name, kind, field, path, code string
		value                         any
	}{
		{"managed", "DHCPv4Server", "managed", "managed", "unsupported_semantics", true},
		{"authoritative", "DHCPv4Server", "authoritative", "authoritative", "unsupported_semantics", true},
		{"log", "DHCPv4Server", "logDHCP", "logDHCP", "unsupported_semantics", true},
		{"router-source", "DHCPv4Server", "routerSource", "routerSource", "unsupported_semantics", "none"},
		{"router", "DHCPv4Server", "router", "router", "unsupported_semantics", "192.0.2.1"},
		{"dns-source", "DHCPv4Server", "dnsSource", "dnsSource", "unsupported_semantics", "none"},
		{"dns-interface", "DHCPv4Server", "dnsInterface", "dnsInterface", "unsupported_semantics", "wan"},
		{"backend", "DHCPv4Server", "server", "server", "unsupported_semantics", "kea"},
		{"relay", "DHCPv4Server", "role", "role", "unsupported_semantics", "transit"},
		{"legacy-journal", "DHCPv4Server", "leaseFile", "leaseFile", "unsupported_semantics", "/" + secret},
		{"embedded-dns", "DHCPv4Server", "dns", "dns", "unsupported_semantics", map[string]any{"enabled": true}},
		{"multi-interface", "DHCPv4Server", "listenInterfaces", "listenInterfaces", "unsupported_semantics", []string{"lan", "wan"}},
		{"qualified-interface", "DHCPv4Server", "interface", "interface", "reference", "Interface/lan"},
		{"kernel-interface-alias", "DHCPv4Server", "interface", "interface", "unsupported_semantics", "eth1"},
		{"shadowed-pool", "DHCPv4Server", "rangeStart", "rangeStart", "ambiguous_value", "192.0.2.101"},
		{"gateway-fallback", "DHCPv4Server", "gatewayFrom", "gatewayFrom", "unsupported_semantics", map[string]any{"resource": "IPv4StaticAddress/gateway", "field": "address"}},
		{"source-no-resource", "DHCPv4Server", "dnsServerFrom", "dnsServerFrom", "required", []any{map[string]any{"field": "address"}}},
		{"source-no-field", "DHCPv4Server", "dnsServerFrom", "dnsServerFrom", "required", []any{map[string]any{"resource": "IPv4StaticAddress/gateway"}}},
		{"source-optional", "DHCPv4Server", "dnsServerFrom", "dnsServerFrom", "unsupported_semantics", []any{map[string]any{"resource": "IPv4StaticAddress/gateway", "field": "address", "optional": true}}},
		{"source-other-status", "DHCPv4Server", "dnsServerFrom", "dnsServerFrom", "unsupported_semantics", []any{map[string]any{"resource": "IPv4StaticAddress/gateway", "field": "dnsServers"}}},
		{"when", "DHCPv4Server", "when", "when", "unsupported_semantics", map[string]any{"state": map[string]any{"VirtualAddress/ha.role": map[string]any{"equals": "master"}}}},
		{"reservation-scope", "DHCPv4Reservation", "scope", "scope", "unsupported_semantics", "phones"},
		{"reservation-lease", "DHCPv4Reservation", "leaseTime", "leaseTime", "unsupported_semantics", "1h"},
		{"reservation-qualified-server", "DHCPv4Reservation", "server", "server", "reference", "DHCPv4Server/lan-v4"},
		{"reservation-missing-server", "DHCPv4Reservation", "server", "server", "reference", secret},
		{"invalid-range", "DHCPv4Server", "addressPool", "spec", "native_validation", map[string]any{"start": "192.0.2.200", "end": "192.0.2.100"}},
		{"infinite-lease", "DHCPv4Server", "addressPool", "spec", "native_validation", map[string]any{"start": "192.0.2.100", "end": "192.0.2.119", "leaseTime": "infinite"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := mutateDHCPInput(t, fixture(t, "dhcp-server"), func(rs []map[string]any) []map[string]any {
				dhcpTestSpec(t, rs, tc.kind)[tc.field] = tc.value
				return rs
			})
			result, err := Compile(data)
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("expected zero result and %s/%s: %+v %v", tc.path, tc.code, result, err)
			}
			raw, _ := json.Marshal(err)
			if strings.Contains(err.Error(), secret) || strings.Contains(string(raw), secret) {
				t.Fatal("DHCP diagnostics exposed a scalar")
			}
		})
	}
}

func TestDHCPMigrationRejectsAmbiguousAddressProfilesAndSelectors(t *testing.T) {
	cases := []struct {
		name, path, code string
		edit             func([]map[string]any) []map[string]any
	}{
		{"unmanaged", "interface", "administrative_state", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "Interface")["managed"] = false
			return rs
		}},
		{"no-static-source", "interface", "ambiguous_reference", func(rs []map[string]any) []map[string]any {
			return append(rs[:1], rs[2:]...)
		}},
		{"ambiguous-static-source", "interface", "ambiguous_reference", func(rs []map[string]any) []map[string]any {
			return append(rs, map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "IPv4StaticAddress", "metadata": map[string]any{"name": "second"}, "spec": map[string]any{"interface": "lan", "address": "192.0.2.2/24"}})
		}},
		{"unused-profile", "profiles", "unreferenced_resource", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			s["profiles"].([]any)[0].(map[string]any)["name"] = "unused"
			return rs
		}},
		{"profile-inline-shadow", "spec", "native_validation", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			s["scopes"].([]any)[0].(map[string]any)["gateway"] = "192.0.2.1"
			return rs
		}},
		{"overlap-pools", "scopes", "ambiguous_value", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			s["scopes"].([]any)[1].(map[string]any)["addressPool"] = map[string]any{"start": "192.0.2.105", "end": "192.0.2.110"}
			return rs
		}},
		{"mac-and-oui-collision", "match", "ambiguous_value", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			s["scopes"].([]any)[1].(map[string]any)["match"] = map[string]any{"macAddresses": []string{"02:aa:bb:00:00:01"}}
			return rs
		}},
		{"secondary-subnet-without-owned-address", "interface", "ambiguous_reference", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			p := s["profiles"].([]any)[0].(map[string]any)
			p["addressPool"], p["gateway"] = map[string]any{"start": "198.51.100.160", "end": "198.51.100.169"}, "198.51.100.1"
			return rs
		}},
		{"reservation-tag-collision", "spec", "ambiguous_identity", func(rs []map[string]any) []map[string]any {
			for _, r := range rs {
				if r["kind"] == "DHCPv4Reservation" {
					r["metadata"].(map[string]any)["name"] = "lan_v4"
				}
			}
			return rs
		}},
		{"range-selector-tag-collision", "scopes", "ambiguous_identity", func(rs []map[string]any) []map[string]any {
			s := dhcpTestSpec(t, rs, "DHCPv4Server")
			s["scopes"].([]any)[1].(map[string]any)["name"] = "phones-match"
			return rs
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Compile(mutateDHCPInput(t, fixture(t, "dhcp-server"), tc.edit))
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("expected %s/%s without partial intent: %v", tc.path, tc.code, err)
			}
		})
	}
}

func TestDHCPGlobalCapacityIncludesReservationsOutsidePools(t *testing.T) {
	base := mutateDHCPInput(t, fixture(t, "dhcp-server"), func(rs []map[string]any) []map[string]any {
		dhcpTestSpec(t, rs, "IPv4StaticAddress")["address"] = "192.0.2.1/23"
		s := dhcpTestSpec(t, rs, "DHCPv4Server")
		delete(s, "profiles")
		delete(s, "scopes")
		s["gateway"] = "192.0.2.1"
		s["addressPool"] = map[string]any{"start": "192.0.2.10", "end": "192.0.3.253"} // 500 addresses
		return append(rs,
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "Interface", "metadata": map[string]any{"name": "guest"}, "spec": map[string]any{"ifname": "eth2", "managed": true, "adminUp": true}},
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "IPv4StaticAddress", "metadata": map[string]any{"name": "guest-address"}, "spec": map[string]any{"interface": "guest", "address": "198.51.100.1/23"}},
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "DHCPv4Server", "metadata": map[string]any{"name": "guest-v4"}, "spec": map[string]any{"interface": "guest", "addressPool": map[string]any{"start": "198.51.100.10", "end": "198.51.101.253"}}},
		)
	})
	// The reservation is currently inside the first pool; no double-counting.
	if _, err := Compile(base); err != nil {
		t.Fatal("exactly 1000 addresses across disjoint workers should fit the old global budget", err)
	}
	outside := mutateDHCPInput(t, base, func(rs []map[string]any) []map[string]any {
		dhcpTestSpec(t, rs, "DHCPv4Reservation")["ipAddress"] = "192.0.2.5"
		return rs
	})
	if result, err := Compile(outside); !hasIssue(err, "addressPool", "unsupported_semantics") || !reflect.DeepEqual(result, Result{}) {
		t.Fatal("splitting workers increased the original shared lease budget", err)
	}
	ambiguous := mutateDHCPInput(t, base, func(rs []map[string]any) []map[string]any {
		delete(dhcpTestSpec(t, rs, "DHCPv4Reservation"), "server")
		return rs
	})
	if _, err := Compile(ambiguous); !hasIssue(err, "server", "ambiguous_reference") {
		t.Fatal("global reservation acquired an arbitrary new worker owner", err)
	}
}

func TestDHCPSelectsEachPoolFromMultipleOwnedSubnets(t *testing.T) {
	input := mutateDHCPInput(t, fixture(t, "dhcp-server"), func(rs []map[string]any) []map[string]any {
		profile := dhcpTestSpec(t, rs, "DHCPv4Server")["profiles"].([]any)[0].(map[string]any)
		profile["addressPool"] = map[string]any{"start": "198.51.100.160", "end": "198.51.100.169"}
		delete(profile, "gateway")
		delete(profile, "dnsServers")
		delete(profile, "netmask")
		server := dhcpTestSpec(t, rs, "DHCPv4Server")
		server["scopes"] = append(server["scopes"].([]any), map[string]any{"name": "other-phones", "profileRef": "voice", "match": map[string]any{"macAddresses": []string{"02:dd:00:00:00:01"}}})
		return append(rs, map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "IPv4StaticAddress", "metadata": map[string]any{"name": "legacy-subnet"}, "spec": map[string]any{"interface": "lan", "address": "198.51.100.1/25"}})
	})
	// The first secondary range is deliberately outside /25. Matching the
	// interface alone must not conceal a pool with no configured subnet.
	if _, err := Compile(input); !hasIssue(err, "interface", "ambiguous_reference") {
		t.Fatal("accepted a secondary pool outside every owned subnet", err)
	}
	input = []byte(strings.ReplaceAll(string(input), "198.51.100.1/25", "198.51.100.129/25"))
	result, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	cfg := service(t, result.Document, "dhcp_server").DHCPServer
	if cfg.ServerAddress != "192.0.2.1" || cfg.Primary.Prefix != "192.0.2.0/24" {
		t.Fatal("secondary address changed the primary server identity")
	}
	for _, scope := range cfg.Scopes {
		if scope.Name == "phones" && (scope.Prefix != "198.51.100.128/25" || scope.Router != "198.51.100.129" || !reflect.DeepEqual(scope.DNSServers, []string{"198.51.100.129"})) {
			t.Fatal("secondary scope lost its own subnet defaults", scope)
		}
	}
}
