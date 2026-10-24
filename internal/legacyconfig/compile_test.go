// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

//go:embed testdata/*.yaml
var fixtures embed.FS

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fixtures.ReadFile("testdata/" + name + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compiled(t *testing.T, name string) Result {
	t.Helper()
	data := fixture(t, name)
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(result.Document); err != nil {
		t.Fatalf("compiler returned invalid native configuration: %v", err)
	}
	// Verify actual native encoding/strict decoding, including DNS's distinct
	// public YAML tags and internal JSON runtime tags.
	encoded, err := json.Marshal(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded config.Document
	if err := config.Decode(bytes.NewReader(encoded), &decoded); err != nil {
		t.Fatalf("native configuration cannot read migration result: %v", err)
	}
	if !reflect.DeepEqual(result.Document, decoded) {
		t.Fatal("native configuration round trip changed migration result")
	}
	second, err := Compile(data)
	if err != nil || !reflect.DeepEqual(result, second) {
		t.Fatal("identical input did not produce deterministic native intent and mappings")
	}
	return result
}

func iface(t *testing.T, d config.Document, name string) network.Interface {
	t.Helper()
	for _, item := range d.Network.Interfaces {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("native interface %s is missing", name)
	return network.Interface{}
}

func service(t *testing.T, d config.Document, role string) serviceconfig.Service {
	t.Helper()
	for _, s := range d.Services {
		if s.Role() == role {
			return s
		}
	}
	t.Fatalf("native service role %s is missing", role)
	return serviceconfig.Service{}
}

func TestStaticAddressRouteAndLinkMigration(t *testing.T) {
	result := compiled(t, "static")
	d := result.Document
	if result.SourceRevision != SourceRevision || result.SourceRouter != "migration-static" || len(result.Mappings) != 12 {
		t.Fatal("migration provenance or resource mappings are incomplete")
	}
	if len(d.Network.Links) != 2 || len(d.Network.Interfaces) != 5 || len(d.Network.Routes) != 2 {
		t.Fatal("network topology was not preserved")
	}
	wan := iface(t, d, "eth0")
	access := iface(t, d, "eth1")
	bridgePort := iface(t, d, "eth2")
	if wan.MTU != 1500 || !reflect.DeepEqual(wan.Addresses, []string{"2001:db8::1/128"}) {
		t.Fatal("WAN MTU/static IPv6 host address was lost")
	}
	if access.Master == nil || *access.Master != "vrf-blue" || !reflect.DeepEqual(access.Addresses, []string{"192.0.2.1/24"}) {
		t.Fatal("VRF membership or IPv4 host bits changed")
	}
	if bridgePort.Master == nil || *bridgePort.Master != "br0" {
		t.Fatal("bridge membership was lost")
	}
	if !reflect.DeepEqual(d.Network.Routes[0], network.Route{Destination: "203.0.113.0/24", Gateway: "192.0.2.254", Interface: "eth1", Metric: 42}) {
		t.Fatal("static IPv4 route changed")
	}
	if d.Network.Routes[1].Gateway != "fe80::1" || d.Network.Routes[1].Interface != "eth0" || d.Network.Routes[1].Metric != 100 {
		t.Fatal("IPv6 next-hop scope or metric was lost")
	}
	if d.Network.Forwarding.IPv4 == nil || !*d.Network.Forwarding.IPv4 || d.Network.Forwarding.IPv6 == nil || *d.Network.Forwarding.IPv6 {
		t.Fatal("explicit forwarding enable/disable settings changed")
	}
	if d.Network.Firewall != nil {
		t.Fatal("migration invented a firewall")
	}
}

func TestWANIdentityRoutesAndHexadecimalDelegation(t *testing.T) {
	d := compiled(t, "wan").Document
	v4 := service(t, d, "dhcp4").DHCP4
	if v4.Interface != "eth0" || v4.DefaultRoute == nil || *v4.DefaultRoute || v4.Metric != 80 || v4.Hostname != "gateway" || v4.ClientID != "01020000000001" {
		t.Fatal("DHCPv4 wire identity or route suppression changed")
	}
	v6 := service(t, d, "dhcp6pd").DHCP6
	if v6.Interface != "eth0" || v6.PrefixLength != 56 || v6.IAID != 1 || v6.ClientDUID != "00030001020000000001" || len(v6.Delegations) != 1 {
		t.Fatal("PD identity/defaults or delegation relationship changed")
	}
	lan := v6.Delegations[0]
	if lan.SubnetID != 16 || lan.HostID != 0x1234 || lan.Interface != "eth1" || lan.Advertise {
		t.Fatal("legacy hexadecimal subnetID, host ID or announcement policy changed")
	}
	data := strings.Replace(string(fixture(t, "wan")), "prefixLength: 56", "prefixLength: 60", 1)
	_, err := Migrate([]byte(data))
	if !hasIssue(err, "subnetID", "invalid_value") {
		t.Fatal("subnet ID that overwrites provider prefix bits was accepted")
	}
}

func TestDNSResourceGraphAndStaticReferences(t *testing.T) {
	d := compiled(t, "dns").Document
	dns := service(t, d, "dns").DNS
	if len(dns.Spec.Listen) != 1 || !reflect.DeepEqual(dns.Spec.Listen[0].Addresses, []string{"192.0.2.1"}) || len(dns.Spec.Listen[0].AddressFrom) != 0 {
		t.Fatal("static listener address reference was not resolved")
	}
	if len(dns.Spec.Sources) != 2 || dns.Spec.Sources[0].Kind != "zone" || dns.Spec.Sources[1].Kind != "upstream" {
		t.Fatal("DNS source order, local zone or default forwarder changed")
	}
	upstream := dns.Spec.Sources[1]
	if !reflect.DeepEqual(upstream.Upstreams, []string{"tls://192.0.2.53:853?serverName=resolver.example", "udp://198.51.100.53:5353"}) ||
		upstream.ViaInterface != "eth0" || upstream.QueryTimeout != "2s" || upstream.Healthcheck.FailThreshold != 3 {
		t.Fatal("DNS transport, certificate identity, interface binding, timeout or health policy changed")
	}
	if len(dns.Zones) != 1 || dns.Zones[0].Spec.Records[0].IPv4 != "192.0.2.1" || dns.Zones[0].Spec.Records[0].IPv4From.Resource != "" || dns.Zones[0].Spec.Records[1].Hostname != "*.lab" {
		t.Fatal("static zone records were not preserved")
	}
	if dns.Spec.Cache.MaxEntries != 1234 || dns.Spec.Cache.NegativeTTL != "30s" || !dns.Spec.QueryLog.Enabled || dns.Spec.QueryLog.Path != "" {
		t.Fatal("cache or native-owned query log policy changed")
	}
}

func TestDSLitePreservesPeerAndOldMTUDefault(t *testing.T) {
	d := compiled(t, "dslite").Document
	if len(d.Network.DSLite) != 1 {
		t.Fatal("DS-Lite tunnel missing")
	}
	tunnel := d.Network.DSLite[0]
	if tunnel.Name != "ds0" || tunnel.Underlay != "eth0" || tunnel.Local != "2001:db8::2" || tunnel.Remote != "2001:db8:ffff::1" || tunnel.MTU != 1460 {
		t.Fatal("tunnel endpoint or fixed legacy MTU changed")
	}
	inner := iface(t, d, "ds0")
	if !reflect.DeepEqual(inner.Addresses, []string{"192.0.0.2/32"}) || inner.AddressOptions["192.0.0.2/32"].Peer != "192.0.0.1/32" {
		t.Fatal("inner point-to-point address semantics were lost")
	}
	if len(d.Network.Routes) != 1 || d.Network.Routes[0].Interface != "ds0" || d.Network.Routes[0].Gateway != "192.0.0.1" {
		t.Fatal("explicit static route device reference changed")
	}
	data := strings.Replace(string(fixture(t, "dslite")), "        tunnelName: ds0\n", "", 1)
	withoutName, err := Migrate([]byte(data))
	if err != nil || withoutName.Network.DSLite[0].Name != "wan" {
		t.Fatalf("legacy tunnel name fallback was not preserved: %v", err)
	}
}

func TestDSLiteLocalAddressFromMeansInnerIPv4(t *testing.T) {
	data := string(fixture(t, "dslite"))
	data = strings.Replace(data, "        localAddressSource: static", "        localAddressFrom: {resource: IPv4StaticAddress/inner, field: address}\n        localAddressSource: static", 1)
	data += "    - apiVersion: net.routerd.net/v1alpha1\n      kind: IPv4StaticAddress\n      metadata: {name: inner}\n      spec: {interface: wan, address: 198.51.100.7/32}\n"
	d, err := Migrate([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if d.Network.DSLite[0].Local != "2001:db8::2" {
		t.Fatal("IPv4 inner source was incorrectly used as outer IPv6 source")
	}
	inner := iface(t, d, "ds0")
	if inner.AddressOptions["198.51.100.7/32"].Peer != "192.0.0.1/32" {
		t.Fatal("explicit inner IPv4 endpoint was not preserved")
	}
}

func hasIssue(err error, path, code string) bool {
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		return false
	}
	for _, issue := range unsupported.Issues {
		if strings.Contains(issue.Path, path) && issue.Code == code {
			return true
		}
	}
	return false
}

func routerWith(resource string) []byte {
	return []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: {name: test}\nspec:\n  resources:\n" + resource)
}

func TestRefusalNeverReturnsPartialIntentOrSecrets(t *testing.T) {
	const secret = "do-not-print-this-password"
	cases := []struct {
		name, resource, path, code string
	}{
		{"unknown-field", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0, managed: true, adminup: true}\n", "adminup", "unknown_field"},
		{"internal-json-field", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: DNSResolver\n      metadata: {name: dns}\n      spec: {listen: [], sources: []}\n", "sources", "unknown_field"},
		{"wireguard", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: WireGuardInterface\n      metadata: {name: wg}\n      spec: {privateKey: " + secret + "}\n", "resources[0].spec.privateKey", "unsupported_semantics"},
		{"vrrp-version", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: VirtualAddress\n      metadata: {name: ha}\n      spec: {family: ipv4, interface: lan, address: 192.0.2.1/32, mode: vrrp, vrrp: {virtualRouterID: 1, authentication: " + secret + "}}\n", "mode", "unsupported_semantics"},
		{"firewall-input", "    - apiVersion: firewall.routerd.net/v1alpha1\n      kind: FirewallPolicy\n      metadata: {name: protect}\n      spec: {sameRoleAccept: true}\n", "resources[0].spec", "unsupported_semantics"},
		{"nat-dynamic-egress", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: NAT44Rule\n      metadata: {name: nat}\n      spec: {type: masquerade, egressPolicyRef: default, sourceRanges: [192.0.2.0/24]}\n", "egressPolicyRef", "unsupported_field"},
		{"dynamic-gating", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0, managed: true, adminUp: true, when: {state: {VirtualAddress/ha.role: {equals: master}}}}\n", "when", "unsupported_semantics"},
		{"owner-lifecycle", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan, ownerRefs: [{kind: Plugin, name: source}]}\n      spec: {ifname: eth0, managed: true, adminUp: true}\n", "ownerRefs", "unsupported_semantics"},
		{"status", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0, managed: false}\n      status: {privateKey: " + secret + "}\n", "status", "observed_state"},
		{"scalar-secret", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0, managed: true, mtu: " + secret + "}\n", "mtu", "yaml_type"},
		{"legacy-default-useDNS", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: DHCPv4Client\n      metadata: {name: wan}\n      spec: {interface: eth0}\n", "useDNS", "unsupported_semantics"},
		{"unsupported-ra-lifetimes", "    - apiVersion: net.routerd.net/v1alpha1\n      kind: IPv6RouterAdvertisement\n      metadata: {name: lan}\n      spec: {interface: eth0, prefix: '2001:db8::/64', validLifetime: 1800}\n", "spec", "unsupported_semantics"},
		{"wrong-version", "    - apiVersion: future.routerd.net/v9\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0}\n", "resources[0]", "unsupported_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Compile(routerWith(tc.resource))
			if !hasIssue(err, tc.path, tc.code) {
				t.Fatalf("missing expected diagnostic %s/%s: %v", tc.path, tc.code, err)
			}
			if !reflect.DeepEqual(result, Result{}) {
				t.Fatal("error returned partially applicable configuration")
			}
			encoded, _ := json.Marshal(err)
			if strings.Contains(err.Error(), secret) || strings.Contains(string(encoded), secret) {
				t.Fatal("migration error leaked a scalar secret")
			}
		})
	}
}

func TestStrictDocumentAndDuplicateHandling(t *testing.T) {
	cases := map[string][]byte{
		"multi-document": append(fixture(t, "wan"), []byte("\n---\n{}\n")...),
		"duplicate":      []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nkind: Router\nmetadata: {name: test}\nspec: {resources: []}\n"),
		"alias":          []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: &meta {name: test}\nspec: {resources: []}\n"),
		"merge":          []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: {name: test, <<: {name: other}}\nspec: {resources: []}\n"),
		"custom-tag":     []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: {name: !secret test}\nspec: {resources: []}\n"),
		"missing-spec":   []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: {name: test}\n"),
		"null-spec":      []byte("apiVersion: routerd.net/v1alpha1\nkind: Router\nmetadata: {name: test}\nspec: null\n"),
		"oversize":       bytes.Repeat([]byte("x"), config.MaxSize+1),
		"syntax-secret":  []byte("password: [do-not-print-this-password\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := Migrate(data)
			if err == nil || !reflect.DeepEqual(d, config.Document{}) {
				t.Fatal("invalid document yielded native intent")
			}
			if strings.Contains(err.Error(), "do-not-print-this-password") {
				t.Fatal("YAML parser error leaked input")
			}
		})
	}
}

func TestAllUnknownFieldsAndUnsupportedKindsAreReported(t *testing.T) {
	data := routerWith("    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: wan}\n      spec: {ifname: eth0, ownerTypo: external, mtuTypo: 9000}\n    - apiVersion: net.routerd.net/v1alpha1\n      kind: TailscaleNode\n      metadata: {name: overlay}\n      spec: {authKey: do-not-print-this-password}\n")
	data = append(data, []byte("unknownRoot: true\n")...)
	_, err := Migrate(data)
	for _, want := range []struct{ path, code string }{
		{"unknownRoot", "unknown_field"}, {"ownerTypo", "unknown_field"}, {"mtuTypo", "unknown_field"}, {"resources[1].spec", "unsupported_kind"},
	} {
		if !hasIssue(err, want.path, want.code) {
			t.Fatalf("diagnostic collection stopped too early: %v", err)
		}
	}
}

func TestSemanticRestrictionsAndDNSFailureAreAtomic(t *testing.T) {
	cases := []struct{ name, fixture, old, replacement, path, code string }{
		{"unmanaged-address", "static", "ifname: eth1, managed: true", "ifname: eth1, managed: false", "members", "administrative_state"},
		{"exclusive-address", "static", "exclusive: false", "exclusive: true", "exclusive", "unsupported_semantics"},
		{"bridge-default-stp", "static", "        stp: false\n", "", "stp", "unsupported_semantics"},
		{"zero-host", "wan", `addressSuffix: "::1234"`, `addressSuffix: "::"`, "addressSuffix", "unsupported_semantics"},
		{"host-only-pd", "wan", "prefixLength: 64", "prefixLength: 128", "prefixLength", "unsupported_semantics"},
		{"implicit-ra", "wan", "sendRA: false", "sendRA: true", "sendRA", "unsupported_semantics"},
		{"dns-path", "dns", "queryLog: {enabled: true}", "queryLog: {enabled: true, path: /tmp/legacy.db}", "queryLog.path", "unsupported_semantics"},
		{"local-dnssec", "dns", "        ttl: 60", "        ttl: 60\n        dnssec: {enabled: true, mode: sign}", "dnssec", "unsupported_semantics"},
		{"dns-missing-upstream", "dns", "DNSUpstream/plain", "DNSUpstream/missing", "upstreams", "reference"},
		{"dns-live-listener", "dns", "resource: IPv4StaticAddress/gateway", "resource: IPv6DelegatedAddress/lan", "addressFrom", "reference"},
		{"dslite-aftr-dns", "dslite", `aftrIPv6: "2001:db8:ffff::1"`, "aftrFQDN: aftr.example", "aftrFQDN", "unsupported_semantics"},
		{"dslite-default-route", "dslite", "encapsulationLimit: none", "encapsulationLimit: none\n        defaultRoute: true", "defaultRoute", "unsupported_semantics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := strings.Replace(string(fixture(t, tc.fixture)), tc.old, tc.replacement, 1)
			d, err := Migrate([]byte(data))
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(d, config.Document{}) {
				t.Fatalf("semantic incompatibility was accepted or misreported: %v", err)
			}
		})
	}
}

func TestObserverOnlyInterfaceDoesNotBecomeOwned(t *testing.T) {
	data := routerWith("    - apiVersion: net.routerd.net/v1alpha1\n      kind: Interface\n      metadata: {name: existing}\n      spec: {ifname: eth0, managed: false, owner: external, adminUp: true}\n")
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Document.Network.Interfaces) != 0 || len(result.Mappings) != 1 || len(result.Mappings[0].Notes) == 0 {
		t.Fatal("observe-only alias silently acquired administrative ownership or disappeared from the mapping report")
	}
}

func FuzzMigrateNeverReturnsPartialDocument(f *testing.F) {
	for _, name := range []string{"static", "wan", "dns", "dslite", "firewall"} {
		data, _ := fixtures.ReadFile("testdata/" + name + ".yaml")
		f.Add(data)
	}
	f.Add([]byte("metadata: {name: [bad}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		result, err := Compile(data)
		if err != nil {
			if !reflect.DeepEqual(result, Result{}) {
				t.Fatal("rejected document returned partial configuration")
			}
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) || len(unsupported.Issues) == 0 {
				t.Fatal("migration error did not carry structured diagnostics")
			}
		} else if err := config.Validate(result.Document); err != nil {
			t.Fatal("successful compilation returned invalid native intent")
		}
	})
}
