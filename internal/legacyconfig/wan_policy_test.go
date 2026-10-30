// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"gopkg.in/yaml.v3"
)

func wanPolicy(t *testing.T) Options {
	t.Helper()
	options, err := DecodeOptions(bytes.NewReader(fixture(t, "wan-dynamic-policy")))
	if err != nil {
		t.Fatal(err)
	}
	return options
}

func compiledPolicy(t *testing.T, data []byte, options Options) Result {
	t.Helper()
	result, err := CompileWithOptions(data, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(result.Document); err != nil {
		t.Fatalf("invalid native result: %v", err)
	}
	raw, err := config.YAML(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded config.Document
	if err := config.Decode(bytes.NewReader(raw), &decoded); err != nil || !reflect.DeepEqual(decoded, result.Document) {
		t.Fatal("native YAML round trip changed policy compilation", err)
	}
	second, err := CompileWithOptions(data, options)
	if err != nil || !reflect.DeepEqual(result, second) {
		t.Fatal("policy compilation is not deterministic", err)
	}
	return result
}

func mapping(t *testing.T, result Result, ref string) Mapping {
	t.Helper()
	for _, item := range result.Mappings {
		if item.Resource == ref {
			return item
		}
	}
	t.Fatalf("missing resource mapping %s", ref)
	return Mapping{}
}

func changed(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("test mutation pattern missing: %s", old)
	}
	return []byte(strings.Replace(string(data), old, replacement, 1))
}

func removeResource(t *testing.T, data []byte, kind, name string) []byte {
	t.Helper()
	var tree yaml.Node
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatal(err)
	}
	resources := child(child(tree.Content[0], "spec"), "resources")
	for i, r := range resources.Content {
		if child(r, "kind").Value == kind && child(child(r, "metadata"), "name").Value == name {
			resources.Content = append(resources.Content[:i], resources.Content[i+1:]...)
			out, err := yaml.Marshal(&tree)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatal("test resource removal target missing")
	return nil
}

func editRA(options *Options, edit func(*serviceconfig.RAClient)) {
	p := options.RAClients["IPv6RAAddress/wan-ra"]
	edit(&p)
	options.RAClients["IPv6RAAddress/wan-ra"] = p
}

func editDS(options *Options, edit func(*DSLitePolicy)) {
	p := options.DSLite["DSLiteTunnel/provider"]
	edit(&p)
	options.DSLite["DSLiteTunnel/provider"] = p
}

func TestDeclaredPDSourcePreservesDSLiteAllocationWithoutExtraPolicy(t *testing.T) {
	data := changed(t, fixture(t, "wan-dynamic"), "localAddressSource: interface", "localAddressSource: delegatedAddress\n        localDelegatedAddress: lan-prefix\n        localAddressSuffix: '::100'")
	options := wanPolicy(t)
	delete(options.DSLite, "DSLiteTunnel/provider")
	result := compiledPolicy(t, data, options)
	pd, ds := service(t, result.Document, "dhcp6"), service(t, result.Document, "aftr")
	if ds.DSLite.SourceFrom != pd.Name || len(pd.DHCP6.Delegations) != 2 {
		t.Fatal("delegated source lost its one PD owner", pd, ds)
	}
	for _, lan := range pd.DHCP6.Delegations {
		if lan.Name == ds.DSLite.SourceDelegation {
			if lan.HostID != 256 || lan.SubnetID != 16 || lan.Interface != "eth1" || lan.PrefixLength != 128 {
				t.Fatal("DS-Lite source suffix changed LAN allocation or enabled extra RA", lan)
			}
			return
		}
	}
	t.Fatal("DS-Lite source has no managed delegated address")
}

func TestExplicitWANPolicyJoinsRAInformationAFTRAndRoute(t *testing.T) {
	result := compiledPolicy(t, fixture(t, "wan-dynamic"), wanPolicy(t))
	d := result.Document
	if len(nativeProgram(t, d).Network.Interfaces) != 2 || len(nativeProgram(t, d).Network.Links) != 0 || len(nativeProgram(t, d).Network.Routes) != 0 {
		t.Fatal("dynamic ownership leaked into static network intent or created an extra socket owner")
	}
	pd, ds := service(t, d, "dhcp6"), service(t, d, "aftr")
	var raService serviceconfig.Service
	for _, candidate := range nativeProgram(t, d).Services {
		if candidate.Name == ds.DSLite.SourceFrom {
			raService = candidate
		}
	}
	if raService.RAClient.Interface != "eth0" || raService.RAClient.Table != 254 || raService.RAClient.Metric != 200 || raService.RAClient.InterfaceID != "::2" {
		t.Fatal("explicit RA identity or route policy changed")
	}
	if !pd.DHCP6.RequestPrefix || pd.DHCP6.IAID != 1 || len(pd.DHCP6.Delegations) != 1 || pd.DHCP6.Delegations[0].SubnetID != 16 {
		t.Fatal("PD was replaced by a second information socket or its delegation changed")
	}
	info := mapping(t, result, "DHCPv6Information/wan-info")
	if !reflect.DeepEqual(info.Targets, []string{"services/" + pd.Name}) || !strings.Contains(strings.Join(info.Notes, " "), "UDP 546") {
		t.Fatal("information alias does not identify its one PD owner")
	}
	got := ds.DSLite
	if got.Interface != "eth0" || got.Tunnel != "ds0" || got.SourceFrom != raService.Name || got.AFTRFrom != pd.Name ||
		!reflect.DeepEqual(got.DNSFrom, []string{pd.Name, raService.Name}) ||
		!reflect.DeepEqual(got.Candidates, []string{"aftr.example", "2001:db8:ffff::1", "2001:db8:ffff::2"}) {
		t.Fatal("AFTR candidate precedence or typed dependencies changed")
	}
	if got.B4Address != "192.0.0.2/32" || got.B4Peer != "192.0.0.1/32" || got.MTU != 1460 || got.DefaultRoute == nil || !*got.DefaultRoute || got.RouteGateway != "192.0.0.1" || got.Metric != 50 {
		t.Fatal("B4 peer, fixed MTU or independently declared route was lost")
	}
	if nativeProgram(t, d).Network.Firewall == nil || len(nativeProgram(t, d).Network.Firewall.NAT) != 1 || nativeProgram(t, d).Network.Firewall.NAT[0].Match.OutInterface != "ds0" {
		t.Fatal("NAT did not retain its separately declared dynamic tunnel interface match")
	}
	for _, ref := range []string{"IPv6RAAddress/wan-ra", "DSLiteTunnel/provider"} {
		if !strings.Contains(strings.Join(mapping(t, result, ref).Notes, " "), "not automatic legacy parity") {
			t.Fatal("policy-driven behavior change was presented as automatic compatibility")
		}
	}
	if !strings.Contains(strings.Join(mapping(t, result, "IPv4Route/ipv4-default").Targets, " "), ds.Name) {
		t.Fatal("default route mapping omitted its new lifetime owner")
	}
}

func TestDefaultCompileDoesNotInferRAOrAFTRPolicy(t *testing.T) {
	result, err := Compile(fixture(t, "wan-dynamic"))
	if !hasIssue(err, "resources[3].spec", "unsupported_semantics") || !hasIssue(err, "aftrFQDN", "unsupported_semantics") || !reflect.DeepEqual(result, Result{}) {
		t.Fatal("default compilation inferred implicit host behavior", err)
	}
	// A policy file with no overrides remains a valid strict wrapper for an
	// already supported static document; it does not loosen that document.
	compiledPolicy(t, fixture(t, "dslite"), Options{SchemaVersion: 1})
}

func TestPDInformationUsesOneIdentityAndExactLegacyInterfaceMatch(t *testing.T) {
	addition := "    - apiVersion: net.routerd.net/v1alpha1\n      kind: DHCPv6Information\n      metadata: {name: info}\n      spec: {interface: wan}\n"
	data := append(fixture(t, "wan"), []byte(addition)...)
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	owners := 0
	for _, candidate := range nativeProgram(t, result.Document).Services {
		if candidate.DHCP6 != nil {
			owners++
		}
	}
	if owners != 1 {
		t.Fatal("information resource opened a second DHCPv6 socket")
	}
	pd := service(t, result.Document, "dhcp6")
	if !reflect.DeepEqual(mapping(t, result, "DHCPv6Information/info").Targets, []string{"services/" + pd.Name}) {
		t.Fatal("information did not alias PD")
	}
	for _, mismatch := range []string{"Interface/wan", "eth0", "lan"} {
		t.Run(mismatch, func(t *testing.T) {
			bad := append(fixture(t, "wan"), []byte(strings.Replace(addition, "interface: wan", "interface: "+mismatch, 1))...)
			if _, err := Compile(bad); !hasIssue(err, "interface", "information_provider") {
				t.Fatal("migration invented an old information-to-PD association", err)
			}
		})
	}
	for _, test := range []struct {
		name       string
		data       []byte
		path, code string
	}{
		{"standalone", removeResource(t, data, "DHCPv6PrefixDelegation", "ipv6-provider"), "interface", "information_provider"},
		{"unknown-request", changed(t, data, "spec: {interface: wan}", "spec: {interface: wan, request: [unknown-option]}"), "request", "unsupported_semantics"},
		{"multiple-pd", append(append([]byte(nil), data...), []byte("    - apiVersion: net.routerd.net/v1alpha1\n      kind: DHCPv6PrefixDelegation\n      metadata: {name: second-pd}\n      spec: {interface: wan}\n")...), "interface", "information_provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := Compile(test.data)
			if !hasIssue(err, test.path, test.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatal("invalid information ownership returned intent", err)
			}
		})
	}
}

func TestPolicyRefusalsAreResourceScopedAndAtomic(t *testing.T) {
	tests := []struct {
		name, path, code string
		mutate           func(*testing.T, []byte, *Options) []byte
	}{
		{"schema", "$policy.schema_version", "policy_version", func(_ *testing.T, d []byte, o *Options) []byte { o.SchemaVersion = 0; return d }},
		{"unknown-ra", "$policy.ra_clients", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			o.RAClients["IPv6RAAddress/missing"] = o.RAClients["IPv6RAAddress/wan-ra"]
			return d
		}},
		{"unqualified-ra", "$policy.ra_clients", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			o.RAClients["wan-ra"] = o.RAClients["IPv6RAAddress/wan-ra"]
			return d
		}},
		{"wrong-kind", "$policy.ra_clients", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			o.RAClients["Interface/wan"] = o.RAClients["IPv6RAAddress/wan-ra"]
			return d
		}},
		{"unknown-ds", "$policy.ds_lite", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			o.DSLite["DSLiteTunnel/missing"] = o.DSLite["DSLiteTunnel/provider"]
			return d
		}},
		{"ra-wrong-interface", "$policy.ra_clients", "policy_interface", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { p.Interface = "eth1" })
			return d
		}},
		{"ra-implicit-feature", "$policy.ra_clients", "policy_required", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { p.OnLink = nil })
			return d
		}},
		{"ra-implicit-metric", "$policy.ra_clients", "policy_required", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { p.Metric = 0 })
			return d
		}},
		{"ra-invalid-iid", "$policy.ra_clients", "policy_validation", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { p.InterfaceID = "2001:db8::2" })
			return d
		}},
		{"ra-observe-only", ".managed", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "interface: wan, managed: true", "interface: wan, managed: false")
		}},
		{"ra-required", ".required", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "required: false", "required: true")
		}},
		{"source-different-kind", "$policy.ds_lite", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.SourceFrom = "DHCPv6PrefixDelegation/wan-pd" })
			return d
		}},
		{"source-no-slaac", "$policy.ds_lite", "reference_scope", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { no := false; p.SLAAC = &no })
			return d
		}},
		{"dns-disabled-ra", "$policy.ds_lite", "policy_source", func(_ *testing.T, d []byte, o *Options) []byte {
			editRA(o, func(p *serviceconfig.RAClient) { no := false; p.DNS = &no })
			return d
		}},
		{"dns-alias-duplicate", "$policy.ds_lite", "duplicate_source", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.DNSFrom = append(p.DNSFrom, "DHCPv6PrefixDelegation/wan-pd") })
			return d
		}},
		{"dns-recursion-source", "$policy.ds_lite", "policy_reference", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.DNSFrom = []string{"DNSResolver/local"} })
			return d
		}},
		{"dns-bootstrap-absent", "$policy.ds_lite", "bootstrap_required", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.Resolvers = nil; p.DNSFrom = nil })
			return d
		}},
		{"dns-loopback", "$policy.ds_lite", "policy_validation", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.Resolvers[0].Address = "::1" })
			return d
		}},
		{"dns-ipv4", "$policy.ds_lite", "policy_validation", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.Resolvers[0].Address = "192.0.2.53" })
			return d
		}},
		{"timing-implicit", "$policy.ds_lite", "policy_required", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.TimeoutSeconds = 0 })
			return d
		}},
		{"selection-implicit", "$policy.ds_lite", "policy_required", func(_ *testing.T, d []byte, o *Options) []byte {
			editDS(o, func(p *DSLitePolicy) { p.Selection = "" })
			return d
		}},
		{"pd-source-policy-conflict", "$policy.ds_lite", "policy_source", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "localAddressSource: interface", "localAddressSource: delegatedAddress\n        localDelegatedAddress: lan-prefix")
		}},
		{"source-suffix", ".localAddressSource", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "localAddressSource: interface", "localAddressSource: interface\n        localAddressSuffix: '::100'")
		}},
		{"missing-ha", ".when", "reference", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "encapsulationLimit: none", "encapsulationLimit: none\n        when: {state: {VirtualAddress/ha.role: {equals: master}}}")
		}},
		{"aftr-default-field", ".aftrFrom", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte { return changed(t, d, ", field: aftrName", "") }},
		{"aftr-optional-field", ".aftrFrom", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "field: aftrName", "field: aftrName, optional: true")
		}},
		{"disabled-tunnel", ".enabled", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "tunnelName: ds0", "tunnelName: ds0\n        enabled: false")
		}},
		{"ignored-default-route", ".defaultRoute", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "tunnelName: ds0", "tunnelName: ds0\n        defaultRoute: true")
		}},
		{"ignored-ordinal", ".aftrAddressSelection", "policy_conflict", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "aftrFQDN: aftr.example", "aftrFQDN: aftr.example\n        aftrAddressOrdinal: 2")
		}},
		{"ignored-resolvers", ".aftrDNSServers", "policy_conflict", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "aftrFQDN: aftr.example", "aftrFQDN: aftr.example\n        aftrDNSServers: [192.0.2.53]")
		}},
		{"nondefault-dynamic-route", ".destination", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "destination: 0.0.0.0/0", "destination: 198.51.100.0/24")
		}},
		{"wrong-peer-route", ".destination", "unsupported_semantics", func(t *testing.T, d []byte, _ *Options) []byte {
			return changed(t, d, "gateway: 192.0.0.1", "gateway: 192.0.0.9")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := wanPolicy(t)
			data := test.mutate(t, fixture(t, "wan-dynamic"), &options)
			result, err := CompileWithOptions(data, options)
			if !hasIssue(err, test.path, test.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("policy refusal %s/%s was lost: %v", test.path, test.code, err)
			}
		})
	}
}

func TestPolicyInputIsNotAliasedByNativeDocument(t *testing.T) {
	options := wanPolicy(t)
	result := compiledPolicy(t, fixture(t, "wan-dynamic"), options)
	before, _ := json.Marshal(result)
	ra := options.RAClients["IPv6RAAddress/wan-ra"]
	*ra.SLAAC = false
	ra.AllowedRouters[0] = "fe80::9"
	ds := options.DSLite["DSLiteTunnel/provider"]
	ds.Resolvers[0].Address = "2001:db8::99"
	ds.DNSFrom[0] = "DHCPv6PrefixDelegation/missing"
	after, _ := json.Marshal(result)
	if !bytes.Equal(before, after) {
		t.Fatal("policy edits mutated already compiled intent")
	}
}

func TestDecodeOptionsRejectsAmbiguousShapeAndHidesScalars(t *testing.T) {
	const secret = "do-not-print-this-password"
	for _, data := range []string{
		"null", "{}", "schema_version: 2", "schema_version: 1\nunknown: " + secret,
		"schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: {interface: eth0, extra: " + secret + "}}",
		"schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: {interface: eth0, table: " + secret + "}}",
		"schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: {Interface: eth0}}",
		"Schema_Version: 1", "schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: null}",
		"schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: {}, IPv6RAAddress/wan-ra: {}}",
		"schema_version: 1\nra_clients: &ra {}", "schema_version: 1\n---\nschema_version: 1",
		"!custom\nschema_version: 1", "schema_version: 1\nra_clients: !custom {}",
		"schema_version: 1\nra_clients: {IPv6RAAddress/wan-ra: {allowed_routers: !custom []}}",
		"schema_version: 1\nds_lite: {DSLiteTunnel/provider: {timeout_seconds: '" + secret + "'}}",
		"schema_version: [" + secret,
	} {
		options, err := DecodeOptions(strings.NewReader(data))
		var issues *UnsupportedError
		if err == nil || !errors.As(err, &issues) || !reflect.DeepEqual(options, Options{}) || strings.Contains(err.Error(), secret) {
			t.Fatal("invalid policy was accepted, returned partial intent or leaked a scalar", err)
		}
	}
	options := wanPolicy(t)
	options.RAClients[secret] = options.RAClients["IPv6RAAddress/wan-ra"]
	_, err := CompileWithOptions(fixture(t, "wan-dynamic"), options)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("unknown resource policy key leaked into diagnostics", err)
	}
}

func TestNativePolicyDoesNotInventDefaultRoutesAndPreservesPeerChoice(t *testing.T) {
	data := removeResource(t, fixture(t, "wan-dynamic"), "IPv4Route", "ipv4-default")
	result := compiledPolicy(t, data, wanPolicy(t))
	ds := service(t, result.Document, "aftr").DSLite
	if ds.DefaultRoute == nil || *ds.DefaultRoute || ds.RouteGateway != "" || ds.B4Peer != "192.0.0.1/32" {
		t.Fatal("dynamic tunnel invented a route absent from the original resource graph")
	}
	data = changed(t, fixture(t, "wan-dynamic"), "        gateway: 192.0.0.1\n", "")
	ds = service(t, compiledPolicy(t, data, wanPolicy(t)).Document, "aftr").DSLite
	if ds.DefaultRoute == nil || !*ds.DefaultRoute || ds.RouteGateway != "" {
		t.Fatal("device-only route acquired an invented peer gateway")
	}
}

func TestExplicitActivationOfLegacyIgnoredAFTRSettings(t *testing.T) {
	options := wanPolicy(t)
	editDS(&options, func(p *DSLitePolicy) { p.Selection, p.Ordinal = "ordinalModulo", 2 })
	data := changed(t, fixture(t, "wan-dynamic"), "aftrFQDN: aftr.example", "aftrFQDN: aftr.example\n        aftrDNSServers: ['2001:db8::53']\n        aftrAddressSelection: ordinalModulo\n        aftrAddressOrdinal: 2")
	result := compiledPolicy(t, data, options)
	ds := service(t, result.Document, "aftr").DSLite
	if ds.Selection != "ordinalModulo" || ds.Ordinal != 2 {
		t.Fatal("explicit activation did not preserve the repeated policy")
	}
	notes := strings.Join(mapping(t, result, "DSLiteTunnel/provider").Notes, " ")
	if !strings.Contains(notes, "ignored by the active Linux controller") || !strings.Contains(notes, "intentionally changes") || !strings.Contains(notes, "direct underlay DNS") {
		t.Fatal("activation of formerly ignored fields was not disclosed")
	}
}

func TestStaticLiteralAFTRNeedsNoDNSAndKeepsInnerAddressSource(t *testing.T) {
	options := Options{SchemaVersion: 1, DSLite: map[string]DSLitePolicy{"DSLiteTunnel/provider": {Selection: "first", TimeoutSeconds: 2, RetrySeconds: 10}}}
	data := fixture(t, "dslite")
	result := compiledPolicy(t, data, options)
	ds := service(t, result.Document, "aftr").DSLite
	if ds.LocalAddress != "2001:db8::2" || ds.SourceFrom != "" || len(ds.Resolvers)+len(ds.DNSFrom) != 0 || !reflect.DeepEqual(ds.Candidates, []string{"2001:db8:ffff::1"}) {
		t.Fatal("literal AFTR mapping invented a DNS dependency or changed source")
	}
	data = changed(t, data, "localAddressSource: static", "localAddressFrom: {resource: IPv4StaticAddress/inner, field: address}\n        localAddressSource: static")
	data = append(data, []byte("    - apiVersion: net.routerd.net/v1alpha1\n      kind: IPv4StaticAddress\n      metadata: {name: inner}\n      spec: {interface: wan, address: 198.51.100.7/32}\n")...)
	ds = service(t, compiledPolicy(t, data, options).Document, "aftr").DSLite
	if ds.B4Address != "198.51.100.7/32" || ds.B4Peer != "192.0.0.1/32" || ds.LocalAddress != "2001:db8::2" {
		t.Fatal("localAddressFrom was confused with the outer IPv6 source")
	}
	options.DSLite["DSLiteTunnel/provider"] = DSLitePolicy{Selection: "first", TimeoutSeconds: 2, RetrySeconds: 10, Resolvers: []serviceconfig.AFTRResolver{{Address: "2001:db8::2"}}}
	if _, err := CompileWithOptions(data, options); !hasIssue(err, "resolvers", "bootstrap_cycle") {
		t.Fatal("AFTR bootstrap accepted a router-owned DNS address", err)
	}
}

func TestCanonicalRAStatusReferenceRequiresMatchingPolicy(t *testing.T) {
	data := changed(t, fixture(t, "wan-dynamic"), "localAddressSource: interface", "localAddressSource: interface\n        localIPv6Source: '${IPv6RAAddress/wan-ra.status.address}'")
	compiledPolicy(t, data, wanPolicy(t))
	data = changed(t, data, "${IPv6RAAddress/wan-ra.status.address}", "${IPv6DelegatedAddress/lan-prefix.status.address}")
	if _, err := CompileWithOptions(data, wanPolicy(t)); !hasIssue(err, "localIPv6Source", "unsupported_semantics") {
		t.Fatal("arbitrary legacy status expression was erased by RA source policy", err)
	}
}

func TestDSLiteInnerSourceUsesAddressField(t *testing.T) {
	base := changed(t, fixture(t, "dslite"), "localAddressSource: static", "localAddressFrom: {resource: IPv4StaticAddress/inner, field: address}\n        localAddressSource: static")
	base = append(base, []byte("    - apiVersion: net.routerd.net/v1alpha1\n      kind: IPv4StaticAddress\n      metadata: {name: inner}\n      spec: {interface: wan, address: 198.51.100.7/32}\n")...)
	virtual := changed(t, base, "IPv4StaticAddress/inner", "VirtualAddress/inner")
	virtual = changed(t, virtual, "kind: IPv4StaticAddress", "kind: VirtualAddress")
	virtual = changed(t, virtual, "spec: {interface: wan, address: 198.51.100.7/32}", "spec: {family: ipv4, interface: wan, address: 198.51.100.7/32, mode: static}")
	policy := Options{SchemaVersion: 1, DSLite: map[string]DSLitePolicy{"DSLiteTunnel/provider": {Selection: "first", TimeoutSeconds: 2, RetrySeconds: 10}}}
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "policy"}[explicit], func(t *testing.T) {
			compile := Compile
			if explicit {
				compile = func(data []byte) (Result, error) { return CompileWithOptions(data, policy) }
			}
			for _, source := range []struct {
				name string
				data []byte
			}{{"static-address", base}, {"virtual-address", virtual}} {
				t.Run(source.name, func(t *testing.T) {
					result, err := compile(source.data)
					if err != nil {
						t.Fatal("explicit static inner source was rejected:", err)
					}
					if explicit {
						if service(t, result.Document, "aftr").DSLite.B4Address != "198.51.100.7/32" {
							t.Fatal("inner IPv4 source changed")
						}
					} else if iface(t, result.Document, "ds0").AddressOptions["198.51.100.7/32"].Peer != "192.0.0.1/32" {
						t.Fatal("fixed inner IPv4 source changed")
					}
					omitted := changed(t, source.data, ", field: address", "")
					if _, err := compile(omitted); err != nil {
						t.Fatal("default address field was rejected:", err)
					}
					bad := changed(t, source.data, ", field: address", ", field: phase")
					result, err = compile(bad)
					if !hasIssue(err, "localAddressFrom.field", "unsupported_semantics") || !reflect.DeepEqual(result, Result{}) {
						t.Fatal("non-address source field was accepted:", err)
					}
				})
			}
		})
	}
}

func TestDSLiteMigrationDoesNotAcceptControllerOnlyEndpointAndMTUForms(t *testing.T) {
	policy := Options{SchemaVersion: 1, DSLite: map[string]DSLitePolicy{"DSLiteTunnel/provider": {Selection: "first", TimeoutSeconds: 2, RetrySeconds: 10}}}
	for _, test := range []struct {
		name, old, replacement, path string
	}{
		{"prefix-source", `localAddress: "2001:db8::2"`, `localAddress: "2001:db8::2/64"`, "localAddress"},
		{"too-small-mtu", "encapsulationLimit: none", "encapsulationLimit: none\n        mtu: 1279", "mtu"},
		{"too-large-mtu", "encapsulationLimit: none", "encapsulationLimit: none\n        mtu: 65496", "mtu"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := changed(t, fixture(t, "dslite"), test.old, test.replacement)
			for _, explicit := range []bool{false, true} {
				var result Result
				var err error
				if explicit {
					result, err = CompileWithOptions(data, policy)
				} else {
					result, err = Compile(data)
				}
				if err == nil || !reflect.DeepEqual(result, Result{}) {
					t.Fatal("invalid public endpoint/MTU became installable intent", explicit, err)
				}
				if !explicit && !hasIssue(err, test.path, "invalid_value") {
					t.Fatal("fixed tunnel lost its scoped source diagnostic", err)
				}
			}
		})
	}
}

func FuzzWANPolicyCompilationRemainsAtomic(f *testing.F) {
	wan, _ := fixtures.ReadFile("testdata/wan-dynamic.yaml")
	policy, _ := fixtures.ReadFile("testdata/wan-dynamic-policy.yaml")
	f.Add(wan, policy)
	f.Add(wan, []byte("schema_version: 1\nra_clients: null"))
	f.Add([]byte("bad: [syntax"), policy)
	f.Fuzz(func(t *testing.T, input, rawPolicy []byte) {
		options, err := DecodeOptions(bytes.NewReader(rawPolicy))
		if err != nil {
			if !reflect.DeepEqual(options, Options{}) {
				t.Fatal("rejected policy returned partial options")
			}
			return
		}
		result, err := CompileWithOptions(input, options)
		if err != nil {
			var issues *UnsupportedError
			if !errors.As(err, &issues) || len(issues.Issues) == 0 || !reflect.DeepEqual(result, Result{}) {
				t.Fatal("policy compilation lost structured atomic failure")
			}
		} else if err := config.Validate(result.Document); err != nil {
			t.Fatal("accepted policy produced invalid native intent", err)
		}
	})
}
