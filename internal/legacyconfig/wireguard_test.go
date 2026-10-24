// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/render"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
)

func legacyWireGuardKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func legacyWireGuardResource(kind, name string, spec map[string]any) map[string]any {
	version := api.NetAPIVersion
	if strings.HasPrefix(kind, "Firewall") {
		version = api.FirewallAPIVersion
	}
	return map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name}, "spec": spec}
}

func legacyWireGuardInput(t *testing.T, resources []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"apiVersion": api.RouterAPIVersion, "kind": "Router", "metadata": map[string]any{"name": "wg-migration"}, "spec": map[string]any{"resources": resources}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func legacyWireGuardResources() []map[string]any {
	return []map[string]any{
		legacyWireGuardResource("WireGuardInterface", "vpn", map[string]any{"ifname": "wg-site", "privateKeyFile": "/var/lib/routerd/wg-site-v1.key", "listenPort": 51820, "mtu": 1412}),
		legacyWireGuardResource("WireGuardPeer", "peer-a", map[string]any{"interface": "vpn", "publicKey": legacyWireGuardKey(17), "allowedIPs": []string{"10.80.0.42/24", "2001:db8:80::/64"}, "endpoint": "198.51.100.2:51821", "persistentKeepalive": 25, "presharedKeyFile": "/var/lib/routerd/wg-peer-a-v1.psk"}),
		legacyWireGuardResource("WireGuardPeer", "peer-b", map[string]any{"interface": "vpn", "publicKey": legacyWireGuardKey(19), "allowedIPs": []string{"10.80.0.128/25"}}),
	}
}

func legacyWireGuardFilter(resources []map[string]any) []map[string]any {
	return append(resources,
		legacyWireGuardResource("Interface", "wan", map[string]any{"ifname": "eth0"}),
		legacyWireGuardResource("FirewallZone", "wan", map[string]any{"role": "untrust", "interfaces": []string{"Interface/wan"}}),
	)
}

func TestWireGuardStaticMigrationPreservesPinnedEngineIntent(t *testing.T) {
	resources := legacyWireGuardResources()
	// References may precede their interface in the original declaration.
	resources[0], resources[1] = resources[1], resources[0]
	resources = append(resources,
		legacyWireGuardResource("IPv4StaticAddress", "inner", map[string]any{"interface": "vpn", "address": "10.80.0.1/24"}),
		legacyWireGuardResource("IPv4StaticRoute", "remote", map[string]any{"interface": "WireGuardInterface/vpn", "destination": "203.0.113.0/24", "metric": 42}),
		legacyWireGuardResource("IPv6StaticRoute", "remote6", map[string]any{"interface": "vpn", "destination": "2001:db8:90::/64", "metric": 43}),
	)
	resources = legacyWireGuardFilter(resources)
	data := legacyWireGuardInput(t, resources)
	result, err := Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(result.Document); err != nil {
		t.Fatal(err)
	}
	n := result.Document.Network
	if len(n.WireGuard) != 1 || len(n.Links) != 1 || n.Links[0].Name != "wg-site" || n.Links[0].Kind != "wireguard" || len(n.WireGuard[0].Peers) != 2 {
		t.Fatal("interface or complete peer set missing")
	}
	if len(n.Routes) != 2 || len(n.PolicyRules) != 0 || n.Routes[0].Destination != "203.0.113.0/24" || n.Routes[0].Interface != "wg-site" || n.Routes[0].Metric != 42 || n.Routes[1].Interface != "wg-site" {
		t.Fatal("explicit route intent changed or AllowedIPs synthesized routing")
	}
	if got := iface(t, result.Document, "wg-site"); got.MTU != 1412 || !reflect.DeepEqual(got.Addresses, []string{"10.80.0.1/24"}) {
		t.Fatal("UP interface/address/MTU projection changed")
	}
	// These values follow the pinned pkg/wireguard.BuildInterface and setconf
	// contract: exact owner name, complete peer replacement, no inferred routes
	// or mark, and optional endpoint/keepalive/key-file references preserved.
	device := n.WireGuard[0]
	if device.Interface != "wg-site" || device.PrivateKeyFile != "/var/lib/routerd/wg-site-v1.key" || device.ListenPort != 51820 || device.FirewallMark != 0 || !device.GeneratePrivateKey {
		t.Fatal("pinned static interface defaults changed")
	}
	wantPeers := []network.WireGuardPeer{
		{PublicKey: legacyWireGuardKey(17), AllowedIPs: []string{"10.80.0.0/24", "2001:db8:80::/64"}, Endpoint: "198.51.100.2:51821", PersistentKeepalive: 25, PresharedKeyFile: "/var/lib/routerd/wg-peer-a-v1.psk"},
		{PublicKey: legacyWireGuardKey(19), AllowedIPs: []string{"10.80.0.128/25"}},
	}
	if !reflect.DeepEqual(device.Peers, wantPeers) {
		t.Fatal("peer endpoint/keepalive/key references, prefix normalization, or nested ownership changed")
	}
	encoded, _ := json.Marshal(result.Document)
	var roundtrip config.Document
	if err := config.Decode(bytes.NewReader(encoded), &roundtrip); err != nil || !reflect.DeepEqual(result.Document, roundtrip) {
		t.Fatal("native document round trip changed WireGuard intent", err)
	}
	second, err := Compile(data)
	if err != nil || !reflect.DeepEqual(result, second) {
		t.Fatal("same input did not produce deterministic complete output", err)
	}
	resources[0], resources[1] = resources[1], resources[0]
	third, err := Compile(legacyWireGuardInput(t, resources))
	if err != nil || !reflect.DeepEqual(result.Document, third.Document) {
		t.Fatal("peer/interface declaration order changed native intent", err)
	}
	if len(result.Mappings) != len(resources) {
		t.Fatal("legacy identity mapping was incomplete")
	}
}

func TestWireGuardMigrationDefaultsArePureAndDoNotInventSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-1.key")
	resources := []map[string]any{legacyWireGuardResource("WireGuardInterface", "wg0", map[string]any{"privateKeyFile": path})}
	result, err := Compile(legacyWireGuardInput(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	n := result.Document.Network
	if len(n.WireGuard) != 1 || !n.WireGuard[0].GeneratePrivateKey || n.WireGuard[0].PrivateKeyFile != path || n.WireGuard[0].ListenPort != 0 || n.WireGuard[0].FirewallMark != 0 || n.Firewall != nil || len(n.Routes) != 0 || len(n.PolicyRules) != 0 {
		t.Fatal("automatic port, absent peers, or explicit key path changed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pure compilation generated/read a key before apply", err)
	}
	if iface(t, result.Document, "wg0").MTU != 0 {
		t.Fatal("omitted MTU was replaced with a guessed value")
	}
	if len(result.Mappings[0].Notes) < 2 || !strings.Contains(strings.Join(result.Mappings[0].Notes, " "), "immutable") {
		t.Fatal("key pinning/provisioning/rotation boundary is not reviewable")
	}
}

func TestWireGuardMigrationNumericIPv6EndpointAndPassivePeer(t *testing.T) {
	resources := legacyWireGuardFilter(legacyWireGuardResources())
	resources[1]["spec"].(map[string]any)["endpoint"] = "[fe80::2%eth9]:51821"
	result, err := Compile(legacyWireGuardInput(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	peers := result.Document.Network.WireGuard[0].Peers
	if peers[0].Endpoint != "[fe80::2%eth9]:51821" || peers[1].Endpoint != "" || peers[1].PersistentKeepalive != 0 {
		t.Fatal("numeric scoped endpoint or passive default changed")
	}
}

func TestWireGuardMigrationRejectsUnsupportedOrAmbiguousIntentAtomically(t *testing.T) {
	const secret = "private-secret-must-never-appear"
	for _, tc := range []struct {
		name, path, code string
		change           func([]map[string]any)
	}{
		{"inline-private", "privateKey", "unsupported_semantics", func(r []map[string]any) { r[0]["spec"].(map[string]any)["privateKey"] = secret }},
		{"missing-private-path", "privateKeyFile", "required", func(r []map[string]any) { delete(r[0]["spec"].(map[string]any), "privateKeyFile") }},
		{"relative-private-path", "privateKeyFile", "required", func(r []map[string]any) { r[0]["spec"].(map[string]any)["privateKeyFile"] = secret + ".key" }},
		{"unclean-private-path", "privateKeyFile", "required", func(r []map[string]any) { r[0]["spec"].(map[string]any)["privateKeyFile"] = "/keys/../" + secret }},
		{"inline-psk", "presharedKey", "unsupported_semantics", func(r []map[string]any) { r[1]["spec"].(map[string]any)["presharedKey"] = secret }},
		{"invalid-psk-path", "presharedKeyFile", "invalid_value", func(r []map[string]any) { r[1]["spec"].(map[string]any)["presharedKeyFile"] = "/keys/" + secret + "\n" }},
		{"hostname", "endpoint", "unsupported_semantics", func(r []map[string]any) { r[1]["spec"].(map[string]any)["endpoint"] = secret + ".example:51820" }},
		{"endpoint-port", "endpoint", "unsupported_semantics", func(r []map[string]any) { r[1]["spec"].(map[string]any)["endpoint"] = "192.0.2.2:0" }},
		{"link-local-zone", "endpoint", "unsupported_semantics", func(r []map[string]any) { r[1]["spec"].(map[string]any)["endpoint"] = "[fe80::2]:51820" }},
		{"endpoint-multicast", "endpoint", "unsupported_semantics", func(r []map[string]any) { r[1]["spec"].(map[string]any)["endpoint"] = "[ff02::1]:51820" }},
		{"dynamic-peers", "peersFrom", "unsupported_semantics", func(r []map[string]any) {
			r[0]["spec"].(map[string]any)["peersFrom"] = []any{map[string]any{"resource": "SAMNodeSet/fabric", "optional": true}}
		}},
		{"sam-self", "selfNodeRef", "unsupported_semantics", func(r []map[string]any) { r[0]["spec"].(map[string]any)["selfNodeRef"] = "self" }},
		{"qualified-peer-owner", "interface", "reference", func(r []map[string]any) { r[1]["spec"].(map[string]any)["interface"] = "WireGuardInterface/vpn" }},
		{"kernel-name-owner", "interface", "reference", func(r []map[string]any) { r[1]["spec"].(map[string]any)["interface"] = "wg-site" }},
		{"missing-peer-owner", "interface", "reference", func(r []map[string]any) { r[1]["spec"].(map[string]any)["interface"] = "absent" }},
		{"public-key", "publicKey", "invalid_value", func(r []map[string]any) { r[1]["spec"].(map[string]any)["publicKey"] = secret }},
		{"zero-key", "publicKey", "invalid_value", func(r []map[string]any) { r[1]["spec"].(map[string]any)["publicKey"] = legacyWireGuardKey(0) }},
		{"duplicate-key", "publicKey", "conflict", func(r []map[string]any) { r[2]["spec"].(map[string]any)["publicKey"] = legacyWireGuardKey(17) }},
		{"duplicate-prefix", "allowedIPs", "conflict", func(r []map[string]any) { r[2]["spec"].(map[string]any)["allowedIPs"] = []string{"10.80.0.99/24"} }},
		{"empty-allowed", "allowedIPs", "required", func(r []map[string]any) { r[1]["spec"].(map[string]any)["allowedIPs"] = []string{} }},
		{"mapped-prefix", "allowedIPs", "invalid_value", func(r []map[string]any) {
			r[1]["spec"].(map[string]any)["allowedIPs"] = []string{"::ffff:192.0.2.0/120"}
		}},
		{"bad-listen-port", "listenPort", "invalid_value", func(r []map[string]any) { r[0]["spec"].(map[string]any)["listenPort"] = -1 }},
		{"bad-mtu", "mtu", "invalid_value", func(r []map[string]any) { r[0]["spec"].(map[string]any)["mtu"] = 500 }},
		{"bad-keepalive", "persistentKeepalive", "invalid_value", func(r []map[string]any) { r[1]["spec"].(map[string]any)["persistentKeepalive"] = 65536 }},
		{"public-fwmark", "fwmark", "unknown_field", func(r []map[string]any) { r[0]["spec"].(map[string]any)["fwmark"] = 23 }},
		{"public-table", "table", "unknown_field", func(r []map[string]any) { r[0]["spec"].(map[string]any)["table"] = 123 }},
		{"when", "when", "unknown_field", func(r []map[string]any) {
			r[0]["spec"].(map[string]any)["when"] = map[string]any{"state": map[string]any{"VirtualAddress/ha.role": map[string]any{"equals": "master"}}}
		}},
		{"depends-on", "dependsOn", "unknown_field", func(r []map[string]any) {
			r[1]["spec"].(map[string]any)["dependsOn"] = []string{"VirtualAddress/ha"}
		}},
		{"ready-when", "ready_when", "unknown_field", func(r []map[string]any) {
			r[0]["spec"].(map[string]any)["ready_when"] = "phase == Ready"
		}},
		{"owner-lifecycle", "ownerRefs", "unsupported_semantics", func(r []map[string]any) {
			r[1]["metadata"].(map[string]any)["ownerRefs"] = []any{map[string]any{"kind": "Plugin", "name": "dynamic"}}
		}},
		{"metadata-annotation", "annotations", "unsupported_metadata", func(r []map[string]any) {
			r[0]["metadata"].(map[string]any)["annotations"] = map[string]string{"dynamic": secret}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := legacyWireGuardFilter(legacyWireGuardResources())
			tc.change(resources)
			result, err := Compile(legacyWireGuardInput(t, resources))
			if !hasIssue(err, tc.path, tc.code) {
				t.Fatalf("missing %s/%s diagnostic: %v", tc.path, tc.code, err)
			}
			if !reflect.DeepEqual(result, Result{}) {
				t.Fatal("rejected graph returned a partially applicable native document")
			}
			encoded, _ := json.Marshal(err)
			if strings.Contains(err.Error(), secret) || strings.Contains(string(encoded), secret) {
				t.Fatal("diagnostic exposed an input scalar")
			}
		})
	}
}

func TestWireGuardMigrationSharedManagedAliasPreservesMTU(t *testing.T) {
	for _, mtu := range []int{0, 1412, 1500} {
		t.Run(fmt.Sprint(mtu), func(t *testing.T) {
			resources := legacyWireGuardFilter(append(legacyWireGuardResources(), legacyWireGuardResource("Interface", "managed", map[string]any{"ifname": "wg-site", "managed": true, "adminUp": true, "mtu": mtu})))
			result, err := Compile(legacyWireGuardInput(t, resources))
			if mtu == 1500 {
				if !hasIssue(err, "mtu", "conflict") || !reflect.DeepEqual(result, Result{}) {
					t.Fatalf("conflicting ownership did not fail atomically: %v", err)
				}
				return
			}
			if err != nil || iface(t, result.Document, "wg-site").MTU != 1412 {
				t.Fatalf("omitted or matching alias MTU erased WireGuard MTU: %v", err)
			}
		})
	}
}

func TestWireGuardMigrationAmbiguousInterfaceOwnershipFailsAtomically(t *testing.T) {
	for _, tc := range []struct {
		name, path, code string
		resource         map[string]any
	}{
		{"kernel-owner", "ifname", "conflict", legacyWireGuardResource("WireGuardInterface", "other", map[string]any{"ifname": "wg-site", "privateKeyFile": "/keys/other.key"})},
		{"alias-owner", "spec", "alias_collision", legacyWireGuardResource("Interface", "vpn", map[string]any{"ifname": "eth0"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := legacyWireGuardFilter(append(legacyWireGuardResources(), tc.resource))
			result, err := Compile(legacyWireGuardInput(t, resources))
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("ambiguous ownership did not fail atomically: %v", err)
			}
		})
	}
}

func TestWireGuardMigrationBoundsAreExplicit(t *testing.T) {
	for _, kind := range []string{"interfaces", "peers", "allowed-ips"} {
		t.Run(kind, func(t *testing.T) {
			resources := []map[string]any{legacyWireGuardResource("WireGuardInterface", "wg0", map[string]any{"privateKeyFile": "/keys/wg0.key"})}
			switch kind {
			case "interfaces":
				for i := 1; i <= network.MaxWireGuardInterfaces; i++ {
					resources = append(resources, legacyWireGuardResource("WireGuardInterface", fmt.Sprintf("wg%d", i), map[string]any{"privateKeyFile": fmt.Sprintf("/keys/wg%d.key", i)}))
				}
			case "peers":
				for i := 0; i <= network.MaxWireGuardPeers; i++ {
					key := [32]byte{byte(i), byte(i >> 8), 1}
					resources = append(resources, legacyWireGuardResource("WireGuardPeer", fmt.Sprintf("peer-%04d", i), map[string]any{"interface": "wg0", "publicKey": base64.StdEncoding.EncodeToString(key[:]), "allowedIPs": []string{fmt.Sprintf("2001:db8::%x/128", i)}}))
				}
			case "allowed-ips":
				prefixes := make([]string, network.MaxWireGuardAllowedIPs+1)
				for i := range prefixes {
					prefixes[i] = fmt.Sprintf("2001:db8::%x/128", i)
				}
				resources = append(resources, legacyWireGuardResource("WireGuardPeer", "peer", map[string]any{"interface": "wg0", "publicKey": legacyWireGuardKey(17), "allowedIPs": prefixes}))
			}
			result, err := Compile(legacyWireGuardInput(t, resources))
			if !hasIssue(err, "resources", "resource_limit") || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("excess %s did not fail atomically with an explicit bound: %v", kind, err)
			}
		})
	}
}

func TestWireGuardMigrationListenerPreservesZoneOrderAndFamily(t *testing.T) {
	resources := legacyWireGuardResources()
	resources = append(resources,
		legacyWireGuardResource("Interface", "wan-a", map[string]any{"ifname": "eth1"}),
		legacyWireGuardResource("Interface", "wan-b", map[string]any{"ifname": "eth5"}),
		legacyWireGuardResource("Interface", "wan-z", map[string]any{"ifname": "eth9"}),
		legacyWireGuardResource("FirewallZone", "z-last", map[string]any{"role": "untrust", "interfaces": []string{"wan-z"}}),
		legacyWireGuardResource("FirewallZone", "a-first", map[string]any{"role": "untrust", "interfaces": []string{"wan-b", "wan-a"}}),
		legacyWireGuardResource("FirewallZone", "mgmt", map[string]any{"role": "mgmt", "interfaces": []string{"WireGuardInterface/vpn"}}),
		legacyWireGuardResource("FirewallRule", "deny-a", map[string]any{"fromZone": "a-first", "toZone": "self", "protocol": "udp", "port": 51820, "action": "drop"}),
		legacyWireGuardResource("FirewallRule", "deny-m", map[string]any{"fromZone": "mgmt", "toZone": "self", "protocol": "udp", "port": 51820, "action": "drop"}),
	)
	result, err := Compile(legacyWireGuardInput(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	f := result.Document.Network.Firewall
	for _, family := range []int{4, 6} {
		for _, in := range []string{"eth5", "eth1", "eth9", "wg-site", "unknown0"} {
			packet := testPacket{in: in, source: "192.0.2.2", destination: "192.0.2.1", protocol: "udp", dport: 51820}
			if family == 6 {
				packet.source, packet.destination = "2001:db8::2", "2001:db8::1"
			}
			want := "drop"
			if in == "eth5" {
				want = "accept"
			}
			if got := packetVerdict(f.Input, packet); got != want {
				t.Fatalf("family %d interface %s: %s, want %s", family, in, got, want)
			}
		}
	}
	if f.Input.Policy != "drop" || f.Output != nil || f.ForwardChain == nil {
		t.Fatal("WireGuard hole changed existing hook policy")
	}
}

func TestWireGuardMigrationStandaloneListenerDoesNotInventStatefulFilter(t *testing.T) {
	for _, nat := range []bool{false, true} {
		t.Run(fmt.Sprint(nat), func(t *testing.T) {
			resources := legacyWireGuardResources()
			if nat {
				resources = append(resources, legacyWireGuardResource("NAT44Rule", "nat", map[string]any{"type": "masquerade", "egressInterface": "eth0", "sourceRanges": []string{"192.0.2.0/24"}}))
			}
			result, err := Compile(legacyWireGuardInput(t, resources))
			if !hasIssue(err, "listenPort", "unsupported_semantics") || !reflect.DeepEqual(result, Result{}) || !strings.Contains(err.Error(), "stateful INPUT drop") {
				t.Fatalf("standalone listener invented stateful packet policy: %v", err)
			}
			resources[0]["spec"].(map[string]any)["listenPort"] = 0
			result, err = Compile(legacyWireGuardInput(t, resources))
			if err != nil {
				t.Fatal("automatic listen port should preserve absent filtering", err)
			}
			f := result.Document.Network.Firewall
			if nat {
				if f == nil || f.Input != nil || f.Output != nil || f.ForwardChain != nil || !f.DisableForward || len(f.NAT) != 1 {
					t.Fatal("automatic-port WireGuard changed the NAT-only hook contract")
				}
			} else if f != nil {
				t.Fatal("automatic-port WireGuard invented filtering")
			}
		})
	}
}

func TestWireGuardMigrationListenerPreservesOriginalBridgeHoleIndex(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			members := []string{"Bridge/a", "Bridge/b"}
			resources := append(legacyWireGuardResources(),
				legacyWireGuardResource("Bridge", "a", map[string]any{"ifname": "br0", "stp": false, "rstp": false}),
				legacyWireGuardResource("Bridge", "b", map[string]any{"ifname": "br1", "stp": false, "rstp": false}),
			)
			if scoped {
				members = append(members, "Interface/wan")
				resources = append(resources, legacyWireGuardResource("Interface", "wan", map[string]any{"ifname": "eth9"}))
			}
			resources = append(resources,
				legacyWireGuardResource("FirewallZone", "wan", map[string]any{"role": "untrust", "interfaces": members}),
				legacyWireGuardResource("FirewallRule", "deny", map[string]any{"fromZone": "wan", "toZone": "self", "protocol": "udp", "port": 51820, "action": "drop"}),
			)
			input := legacyWireGuardInput(t, resources)
			result, err := Compile(input)
			if err != nil {
				t.Fatal(err)
			}
			// Execute the pinned renderer, including its empty variadic string
			// and compaction behavior. An absent hole-index name must not be
			// mistaken for an unscoped permit during native migration.
			_, decoded, diagnostics := decode(input)
			if err := diagnostics.err(); err != nil {
				t.Fatal(err)
			}
			router := &api.Router{}
			for _, r := range decoded {
				router.Spec.Resources = append(router.Spec.Resources, r.Resource)
			}
			holes := render.InternalFirewallHoles(router)
			wantScope := []string{""}
			if scoped {
				wantScope = []string{"eth9"}
			}
			if len(holes) != 1 || holes[0].FromZone != "wan" || holes[0].ToZone != "self" || holes[0].Protocol != "udp" || holes[0].Port != 51820 || !reflect.DeepEqual(holes[0].IfNames, wantScope) {
				t.Fatal("pinned WireGuard hole-index behavior changed", holes)
			}
			rendered, err := render.NftablesFirewall(router, holes)
			if err != nil || !strings.Contains(string(rendered), "iifname \""+wantScope[0]+"\" udp dport 51820 counter accept comment") {
				t.Fatal("pinned renderer changed the scoped or empty ingress match", err)
			}
			if !scoped && !strings.Contains(strings.Join(mapping(t, result, "WireGuardInterface/vpn").Notes, " "), "empty iifname") {
				t.Fatal("ineffective original WireGuard listener rule was not reported")
			}
			for _, source := range []string{"192.0.2.2", "2001:db8::2"} {
				for _, in := range []string{"br0", "br1", "eth9", "other0"} {
					packet := testPacket{in: in, source: source, destination: source, protocol: "udp", dport: 51820}
					want := "drop"
					if scoped && in == "eth9" {
						want = "accept"
					}
					if got := packetVerdict(result.Document.Network.Firewall.Input, packet); got != want {
						t.Fatalf("scoped=%t interface=%s source=%s got=%s want=%s", scoped, in, source, got, want)
					}
				}
			}
		})
	}
}

func TestWireGuardMigrationWithoutUntrustZonePreservesExplicitDeny(t *testing.T) {
	resources := append(legacyWireGuardResources(),
		legacyWireGuardResource("FirewallZone", "mgmt", map[string]any{"role": "mgmt", "interfaces": []string{"WireGuardInterface/vpn"}}),
		legacyWireGuardResource("FirewallRule", "deny", map[string]any{"fromZone": "mgmt", "toZone": "self", "protocol": "udp", "port": 51820, "action": "drop"}),
	)
	result, err := Compile(legacyWireGuardInput(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"192.0.2.2", "2001:db8::2"} {
		packet := testPacket{in: "wg-site", source: source, destination: source, protocol: "udp", dport: 51820}
		if got := packetVerdict(result.Document.Network.Firewall.Input, packet); got != "drop" {
			t.Fatal("WireGuard listener bypassed explicit management-zone denial")
		}
	}
}
