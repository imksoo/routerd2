// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"encoding/json"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func dhcpDNSPolicy(t *testing.T) Options {
	t.Helper()
	options, err := DecodeOptions(bytes.NewReader(fixture(t, "dhcp-dns-policy")))
	if err != nil {
		t.Fatal(err)
	}
	return options
}

func TestDHCPDNSReplacementRetainsStaticGraphAndScopedProducerIdentity(t *testing.T) {
	data := fixture(t, "dhcp-dns")
	if result, err := Compile(data); !hasIssue(err, "spec", "explicit_policy_required") || !reflect.DeepEqual(result, Result{}) {
		t.Fatal("global legacy DHCP events were silently reinterpreted as scoped finite native ownership", err)
	}
	options := dhcpDNSPolicy(t)
	result := compiledPolicy(t, data, options)
	dhcp := service(t, result.Document, "dhcp_server")
	dns := service(t, result.Document, "dns")
	if !dnsconfig.NeedsInput(dns.DNS.RuntimeConfig) || len(dns.DNS.Zones) != 1 || !reflect.DeepEqual(dns.DNS.Zones[0].Spec.DHCPDerived.Sources, []string{"DHCPv4Server/" + dhcp.Name}) {
		t.Fatal("source reference did not join the compiled DHCP producer", dns.DNS)
	}
	zone := dns.DNS.Zones[0].Spec
	if zone.DHCPDerived.LeaseFile != "" || !zone.DHCPDerived.DDNS || zone.Zone != "home.arpa" || zone.DHCPDerived.TTL != 45 || zone.TTL != 120 || len(zone.Records) != 1 || zone.Records[0].Hostname != "gateway" || len(zone.ReverseZones) != 1 {
		t.Fatal("joining dynamic leases changed static records, reverse zones or TTLs", zone)
	}
	if dhcp.DHCPServer.Primary.Domain != "home.arpa" || dhcp.DHCPServer.Primary.Router != "192.0.2.1" || !reflect.DeepEqual(dhcp.DHCPServer.Primary.DNSServers, []string{"192.0.2.1"}) {
		t.Fatal("static DNS zone/address references were not preserved in DHCP options", dhcp.DHCPServer.Primary)
	}
	if !strings.Contains(strings.Join(mapping(t, result, "DNSResolver/resolver").Notes, " "), "Explicit DHCP DNS replacement") || !strings.Contains(strings.Join(mapping(t, result, "DNSZone/home").Notes, " "), "never frozen") {
		t.Fatal("policy and dynamic lease boundary were not made reviewable")
	}
	// A successful document cannot retain caller-owned policy pointers.
	*options.DHCPDNS["DNSResolver/resolver"].SourceScopedFiniteLeases = false
	if err := config.Validate(result.Document); err != nil {
		t.Fatal("caller edit mutated compiled DNS intent", err)
	}
	if _, err := CompileWithOptions(data, options); !hasIssue(err, "source_scoped_finite_leases", "explicit_policy_required") {
		t.Fatal("explicit false replacement was accepted", err)
	}
}

func TestDHCPDNSRejectsUnmigratedSourcesAndIgnoredFields(t *testing.T) {
	cases := []struct {
		name, path, code string
		edit             func([]map[string]any) []map[string]any
	}{
		{"missing-source", "dhcpDerived.sources", "reference", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"] = map[string]any{"sources": []string{"DHCPv4Server/missing"}}
			return rs
		}},
		{"wrong-kind", "dhcpDerived.sources", "reference", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"] = map[string]any{"sources": []string{"DNSResolver/resolver"}}
			return rs
		}},
		{"aliased-duplicate", "dhcpDerived.sources", "reference", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"] = map[string]any{"sources": []string{"lan-v4", "DHCPv4Server/lan-v4"}}
			return rs
		}},
		{"failed-producer", "dhcpDerived.sources", "reference", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DHCPv4Server")["authoritative"] = true
			return rs
		}},
		{"legacy-file", "dhcpDerived.leaseFile", "unsupported_semantics", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"] = map[string]any{"leaseFile": "/private/legacy.leases"}
			return rs
		}},
		{"suffix-ignored-upstream", "dhcpDerived", "unsupported_semantics", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"].(map[string]any)["hostnameSuffix"] = ".home.arpa"
			return rs
		}},
		{"unused-derived-ttl", "dhcpDerived.ttl", "unreferenced_resource", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DNSZone")["dhcpDerived"] = map[string]any{"ttl": 45}
			return rs
		}},
		{"domain-source-optional", "domainFrom", "unsupported_semantics", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DHCPv4Server")["domainFrom"].(map[string]any)["optional"] = true
			return rs
		}},
		{"domain-source-field", "domainFrom", "unsupported_semantics", func(rs []map[string]any) []map[string]any {
			dhcpTestSpec(t, rs, "DHCPv4Server")["domainFrom"].(map[string]any)["field"] = "phase"
			return rs
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := mutateDHCPInput(t, fixture(t, "dhcp-dns"), tc.edit)
			result, err := CompileWithOptions(data, dhcpDNSPolicy(t))
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("replacement policy hid unsupported legacy semantics: %s/%s %v", tc.path, tc.code, err)
			}
		})
	}
}

func TestDHCPDNSPolicyShapeIdentityAndUnusedEntries(t *testing.T) {
	cases := []struct{ name, policy, path, code string }{
		{"wrong-version", "schema_version: 2\ndhcp_dns: {DNSResolver/resolver: {source_scoped_finite_leases: true}}\n", "schema_version", "policy_version"},
		{"wrong-case", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {SourceScopedFiniteLeases: true}}\n", "$policy", "policy_decode"},
		{"null-object", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: null}\n", "$policy", "policy_decode"},
		{"extra-field", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {source_scoped_finite_leases: true, ignore_errors: true}}\n", "$policy", "policy_decode"},
		{"duplicate-key", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {source_scoped_finite_leases: true, source_scoped_finite_leases: true}}\n", "$policy", "policy_decode"},
		{"unqualified-ref", "schema_version: 1\ndhcp_dns: {resolver: {source_scoped_finite_leases: true}}\n", "$policy.dhcp_dns", "policy_reference"},
		{"wrong-kind", "schema_version: 1\ndhcp_dns: {DHCPv4Server/lan-v4: {source_scoped_finite_leases: true}}\n", "$policy.dhcp_dns", "policy_reference"},
		{"missing-ref", "schema_version: 1\ndhcp_dns: {DNSResolver/missing: {source_scoped_finite_leases: true}}\n", "$policy.dhcp_dns", "policy_reference"},
		{"omitted-flag", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {}}\n", "source_scoped_finite_leases", "explicit_policy_required"},
		{"null-flag", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {source_scoped_finite_leases: null}}\n", "source_scoped_finite_leases", "explicit_policy_required"},
		{"false-flag", "schema_version: 1\ndhcp_dns: {DNSResolver/resolver: {source_scoped_finite_leases: false}}\n", "source_scoped_finite_leases", "explicit_policy_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options, err := DecodeOptions(strings.NewReader(tc.policy))
			if err == nil {
				var result Result
				result, err = CompileWithOptions(fixture(t, "dhcp-dns"), options)
				if !reflect.DeepEqual(result, Result{}) {
					t.Fatal("invalid replacement policy returned a usable document")
				}
			}
			if !hasIssue(err, tc.path, tc.code) {
				t.Fatalf("expected %s/%s: %v", tc.path, tc.code, err)
			}
		})
	}
	data := mutateDHCPInput(t, fixture(t, "dhcp-dns"), func(rs []map[string]any) []map[string]any {
		delete(dhcpTestSpec(t, rs, "DNSZone"), "dhcpDerived")
		return rs
	})
	if result, err := CompileWithOptions(data, dhcpDNSPolicy(t)); !hasIssue(err, "$policy.dhcp_dns", "unused_policy") || !reflect.DeepEqual(result, Result{}) {
		t.Fatal("policy for a resolver with no DHCP dependency was consumed", err)
	}
	// Programmatic callers are subject to the same aggregate policy limit.
	options := dhcpDNSPolicy(t)
	for i := 0; i < 4096; i++ {
		raw, _ := json.Marshal(i)
		options.DHCPDNS["DNSResolver/extra-"+string(raw)] = DHCPDNSPolicy{}
	}
	if _, err := CompileWithOptions(fixture(t, "dhcp-dns"), options); !hasIssue(err, "$policy", "policy_limit") {
		t.Fatal("new policy entries bypassed aggregate resource bound", err)
	}
}

func TestDHCPDNSPolicyMustCoverEachResolverReceivingLeaseZones(t *testing.T) {
	data := mutateDHCPInput(t, fixture(t, "dhcp-dns"), func(rs []map[string]any) []map[string]any {
		return append(rs,
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "DNSResolver", "metadata": map[string]any{"name": "second"}, "spec": map[string]any{"listen": []any{map[string]any{"name": "local", "addresses": []string{"127.0.0.1"}, "port": 5353, "sources": []string{"second-zone"}}}}},
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "DNSForwarder", "metadata": map[string]any{"name": "second-zone"}, "spec": map[string]any{"resolver": "second", "match": []string{"home.arpa"}, "zoneRefs": []string{"home"}}},
		)
	})
	options := dhcpDNSPolicy(t)
	if result, err := CompileWithOptions(data, options); !hasIssue(err, "resources[6].spec", "explicit_policy_required") || !reflect.DeepEqual(result, Result{}) {
		t.Fatal("one resolver's policy implicitly authorized another resolver", err)
	}
	yes := true
	options.DHCPDNS["DNSResolver/second"] = DHCPDNSPolicy{SourceScopedFiniteLeases: &yes}
	result := compiledPolicy(t, data, options)
	consumers := 0
	for _, s := range nativeProgram(t, result.Document).Services {
		if s.DNS != nil && dnsconfig.NeedsInput(s.DNS.RuntimeConfig) {
			consumers++
		}
	}
	if consumers != 2 {
		t.Fatal("explicit policies did not independently authorize both consumers")
	}
}
