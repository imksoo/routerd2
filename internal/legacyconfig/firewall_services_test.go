// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/render"
	"github.com/imksoo/routerd2/internal/network"
)

func serviceFirewallResource(kind, name string, spec map[string]any) map[string]any {
	version := api.NetAPIVersion
	if strings.HasPrefix(kind, "Firewall") {
		version = api.FirewallAPIVersion
	}
	return map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name}, "spec": spec}
}

func serviceFirewallInput(t *testing.T, resources []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"apiVersion": api.RouterAPIVersion, "kind": "Router", "metadata": map[string]any{"name": "service-firewall"}, "spec": map[string]any{"resources": resources}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func serviceFirewallResources(role string, bridge bool) []map[string]any {
	link := serviceFirewallResource("Interface", "lan", map[string]any{"ifname": "eth1", "managed": true, "adminUp": true})
	ref := "Interface/lan"
	if bridge {
		link = serviceFirewallResource("Bridge", "lan", map[string]any{"stp": false, "rstp": false})
		ref = "Bridge/lan"
	}
	return []map[string]any{
		link,
		serviceFirewallResource("Interface", "spare", map[string]any{"ifname": "eth2", "managed": true, "adminUp": true}),
		serviceFirewallResource("Interface", "outside", map[string]any{"ifname": "eth9", "managed": true, "adminUp": true}),
		serviceFirewallResource("IPv4StaticAddress", "gateway", map[string]any{"interface": "lan", "address": "192.0.2.1/24"}),
		serviceFirewallResource("DHCPv4Server", "server", map[string]any{"interface": "lan", "listenInterfaces": []string{"lan"}, "addressPool": map[string]any{"start": "192.0.2.100", "end": "192.0.2.119", "leaseTime": "1h"}}),
		serviceFirewallResource("FirewallZone", "inside", map[string]any{"role": role, "interfaces": []string{ref, "Interface/spare"}}),
		serviceFirewallResource("FirewallZone", "outside", map[string]any{"role": role, "interfaces": []string{"Interface/outside"}}),
		serviceFirewallResource("FirewallRule", "a-deny-dhcp", map[string]any{"fromZone": "inside", "toZone": "self", "protocol": "udp", "port": 67, "action": "drop"}),
		serviceFirewallResource("FirewallRule", "z-deny-inside", map[string]any{"fromZone": "inside", "toZone": "self", "action": "drop"}),
		serviceFirewallResource("FirewallRule", "z-deny-outside", map[string]any{"fromZone": "outside", "toZone": "self", "action": "drop"}),
	}
}

func serviceFirewallWithout(resources []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range resources {
		if r["kind"] != kind {
			out = append(out, r)
		}
	}
	return out
}

func serviceFirewallLegacy(t *testing.T, input []byte) (*api.Router, []render.FirewallHole) {
	t.Helper()
	_, resources, d := decode(input)
	if err := d.err(); err != nil {
		t.Fatal(err)
	}
	router := &api.Router{}
	for _, r := range resources {
		router.Spec.Resources = append(router.Spec.Resources, r.Resource)
	}
	return router, render.InternalFirewallHoles(router)
}

func TestFirewallDHCPServerExceptionsPreserveOrderedPacketDecisions(t *testing.T) {
	for _, role := range []string{"untrust", "trust", "mgmt"} {
		for _, bridge := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bridge=%t", role, bridge), func(t *testing.T) {
				resources := serviceFirewallResources(role, bridge)
				input := serviceFirewallInput(t, resources)
				result, err := Compile(input)
				if err != nil {
					t.Fatal(err)
				}
				baseline, err := Compile(serviceFirewallInput(t, serviceFirewallWithout(resources, "DHCPv4Server")))
				if err != nil {
					t.Fatal(err)
				}
				f, old := result.Document.Network.Firewall, baseline.Document.Network.Firewall
				if f.Input.Policy != old.Input.Policy || !reflect.DeepEqual(f.Output, old.Output) || !reflect.DeepEqual(f.ForwardChain, old.ForwardChain) || !reflect.DeepEqual(f.Input.Rules[:2], old.Input.Rules[:2]) {
					t.Fatal("service exception changed other hooks, defaults or global precedence")
				}
				router, holes := serviceFirewallLegacy(t, input)
				if len(holes) != 1 || holes[0].Protocol != "udp" || holes[0].Port != 67 || holes[0].FromZone != "inside" || holes[0].ToZone != "self" {
					t.Fatal("fixture no longer matches pinned DHCP hole selection", holes)
				}
				wantIfNames := []string{"eth1"}
				if bridge {
					wantIfNames = []string{""}
				}
				if !reflect.DeepEqual(holes[0].IfNames, wantIfNames) {
					t.Fatal("pinned Interface/Bridge hole-index distinction changed", holes)
				}
				rendered, err := render.NftablesFirewall(router, holes)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(rendered), "/DHCPv4Server/server\"") != (role == "untrust") {
					t.Fatal("pinned renderer no longer suppresses implicit-accept service holes")
				}
				if bridge && role == "untrust" {
					if !strings.Contains(string(rendered), "iifname \"\" udp dport 67 counter accept comment") {
						t.Fatal("pinned Bridge hole no longer has an ineffective empty ingress match")
					}
					if !strings.Contains(strings.Join(mapping(t, result, "DHCPv4Server/server").Notes, " "), "empty iifname") {
						t.Fatal("ineffective legacy Bridge hole was not reported")
					}
				}
				lan := "eth1"
				if bridge {
					lan = "lan"
				}
				for _, in := range []string{lan, "eth2", "eth9", "unknown0"} {
					for _, family := range []int{4, 6} {
						for _, proto := range []string{"udp", "tcp"} {
							for _, port := range []int{53, 67, 68, 546} {
								p := testPacket{in: in, source: "192.0.2.20", destination: "255.255.255.255", protocol: proto, sport: 12345, dport: port}
								if family == 6 {
									p.source, p.destination = "2001:db8::20", "2001:db8::1"
								}
								want := "drop"
								if role == "untrust" && !bridge && in == lan && proto == "udp" && port == 67 {
									want = "accept"
								}
								if got := packetVerdict(f.Input, p); got != want {
									t.Fatalf("family %d interface %s %s/%d: %s, want %s", family, in, proto, port, got, want)
								}
							}
						}
					}
				}
			})
		}
	}
}

func TestFirewallDHCPServerZoneLookupDoesNotInventResourceAliases(t *testing.T) {
	for _, tc := range []struct {
		ref      string
		wantHole bool
		rejected bool
	}{
		{"Interface/lan", true, false}, {"lan", true, false},
		{"eth1", false, true}, {"Interface/eth1", false, true},
		{"Interface/ lan", false, true}, {"Interface/lan ", true, true},
		{" lan", true, true}, {"lan ", true, true},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			resources := serviceFirewallResources("untrust", false)
			resources[5]["spec"].(map[string]any)["interfaces"] = []string{tc.ref, "Interface/spare"}
			input := serviceFirewallInput(t, resources)
			result, err := Compile(input)
			router, holes := serviceFirewallLegacy(t, input)
			if (len(holes) > 0) != tc.wantHole {
				t.Fatal("pinned resource index differs from expected alias boundary", holes)
			}
			_, renderErr := render.NftablesFirewall(router, holes)
			if (renderErr != nil) != tc.rejected {
				t.Fatal("pinned filter alias lookup differs from expected boundary", renderErr)
			}
			if tc.rejected {
				if !hasIssue(err, "interfaces", "reference") || !reflect.DeepEqual(result, Result{}) {
					t.Fatal("invalid legacy zone reference was silently normalized", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			packet := testPacket{in: "eth1", source: "192.0.2.20", destination: "192.0.2.1", protocol: "udp", sport: 68, dport: 67}
			if got := packetVerdict(result.Document.Network.Firewall.Input, packet); got != "accept" {
				t.Fatalf("zone resource %q: %s, want accept", tc.ref, got)
			}
		})
	}
}

func TestFirewallDNSLiteralAndResolvedListenersNeverBypassACLs(t *testing.T) {
	for _, role := range []string{"untrust", "trust", "mgmt"} {
		for _, source := range []string{"literal", "wildcard", "loopback", "addressFrom", "loopback-and-addressFrom"} {
			t.Run(role+"/"+source, func(t *testing.T) {
				resources := serviceFirewallWithout(serviceFirewallResources(role, false), "DHCPv4Server")
				listen := map[string]any{"name": "local", "port": 5353, "sources": []string{"local"}}
				switch source {
				case "literal":
					listen["addresses"] = []string{"192.0.2.1"}
				case "wildcard":
					listen["addresses"] = []string{"0.0.0.0", "::"}
				case "loopback":
					listen["addresses"] = []string{"127.0.0.1", "::1"}
				case "loopback-and-addressFrom":
					listen["addresses"] = []string{"127.0.0.1", "::1"}
					fallthrough
				case "addressFrom":
					listen["addressFrom"] = []map[string]any{{"resource": "IPv4StaticAddress/gateway", "field": "address"}}
				}
				resources = append(resources,
					serviceFirewallResource("DNSResolver", "resolver", map[string]any{"listen": []map[string]any{listen}}),
					serviceFirewallResource("DNSZone", "home", map[string]any{"zone": "home.arpa", "records": []map[string]any{{"hostname": "router", "ipv4": "192.0.2.1"}}}),
					serviceFirewallResource("DNSForwarder", "local", map[string]any{"resolver": "resolver", "match": []string{"home.arpa"}, "zoneRefs": []string{"DNSZone/home"}}))
				input := serviceFirewallInput(t, resources)
				result, err := Compile(input)
				if err != nil {
					t.Fatal(err)
				}
				router, holes := serviceFirewallLegacy(t, input)
				wantCandidates := role != "untrust" && (source == "literal" || source == "wildcard")
				if (len(holes) > 0) != wantCandidates {
					t.Fatal("literal-address and addressFrom legacy hole selection changed", holes)
				}
				rendered, err := render.NftablesFirewall(router, holes)
				if err != nil || strings.Contains(string(rendered), "/DNSResolver/resolver\"") {
					t.Fatal("legacy renderer unexpectedly activated a DNS hole", err)
				}
				for _, in := range []string{"eth1", "eth2", "eth9", "unknown0"} {
					for _, proto := range []string{"tcp", "udp"} {
						for _, port := range []int{53, 5353} {
							for _, source := range []string{"192.0.2.20", "2001:db8::20"} {
								p := testPacket{in: in, source: source, destination: source, protocol: proto, dport: port}
								if got := packetVerdict(result.Document.Network.Firewall.Input, p); got != "drop" {
									t.Fatalf("DNS listener bypassed ACL for %+v: %s", p, got)
								}
							}
						}
					}
				}
				if !strings.Contains(strings.Join(mapping(t, result, "DNSResolver/resolver").Notes, " "), "INPUT ACL decisions") {
					t.Fatal("DNS firewall behavior was not reported")
				}
			})
		}
	}
}

func TestFirewallServiceExceptionProjectionLimitIsAtomic(t *testing.T) {
	input := serviceFirewallInput(t, serviceFirewallResources("untrust", false))
	_, resources, d := decode(input)
	c := &compiler{decoder: d, resources: resources, byRef: map[string]*resource{}, mappings: map[string]*Mapping{}, aliases: map[string]string{}, aliasOwner: map[string]string{}, upAllowed: map[string]bool{}}
	for i := range c.resources {
		r := &c.resources[i]
		c.byRef[r.Kind+"/"+r.Metadata.Name] = r
		c.mappings[r.ID()] = &Mapping{}
	}
	c.collectAliases()
	chain := &network.FirewallChain{Policy: "drop", Rules: []network.FirewallRule{{Match: network.PacketMatch{InInterface: "lo"}, Action: "accept"}, {Match: network.PacketMatch{Family: 6, Protocol: "icmpv6"}, Action: "accept"}}}
	for len(chain.Rules) < maxFirewallProjection {
		chain.Rules = append(chain.Rules, network.FirewallRule{Match: network.PacketMatch{InInterface: "eth1"}, Action: "drop"})
	}
	c.document.Network.Firewall = &network.Firewall{Input: chain}
	before := append([]network.FirewallRule(nil), chain.Rules...)
	c.firewallServices()
	if !hasIssue(c.err(), "resources", "resource_limit") || !reflect.DeepEqual(before, chain.Rules) {
		t.Fatal("service exceptions exceeded their rule bound or partially changed the chain", c.err())
	}
}

func TestFirewallServiceExceptionsDoNotCreateFilteringHooks(t *testing.T) {
	for _, nat := range []bool{false, true} {
		t.Run(fmt.Sprintf("nat=%t", nat), func(t *testing.T) {
			resources := serviceFirewallWithout(serviceFirewallWithout(serviceFirewallResources("untrust", false), "FirewallZone"), "FirewallRule")
			resources = append(resources,
				serviceFirewallResource("DNSResolver", "resolver", map[string]any{"listen": []map[string]any{{"name": "local", "addresses": []string{"192.0.2.1"}, "port": 5353, "sources": []string{"local"}}}}),
				serviceFirewallResource("DNSZone", "home", map[string]any{"zone": "home.arpa", "records": []map[string]any{{"hostname": "router", "ipv4": "192.0.2.1"}}}),
				serviceFirewallResource("DNSForwarder", "local", map[string]any{"resolver": "resolver", "match": []string{"home.arpa"}, "zoneRefs": []string{"DNSZone/home"}}))
			if nat {
				resources = append(resources, serviceFirewallResource("NAT44Rule", "outbound", map[string]any{"type": "masquerade", "egressInterface": "eth9", "sourceRanges": []string{"192.0.2.0/24"}}))
			}
			input := serviceFirewallInput(t, resources)
			result, err := Compile(input)
			if err != nil {
				t.Fatal(err)
			}
			router, holes := serviceFirewallLegacy(t, input)
			rendered, err := render.NftablesFirewall(router, holes)
			if err != nil || len(holes) != 0 || len(rendered) != 0 {
				t.Fatal("standalone fixture unexpectedly enables the legacy filter", err, holes)
			}
			f := result.Document.Network.Firewall
			if !nat && f != nil || nat && (f == nil || !f.DisableForward || f.Input != nil || f.Output != nil || f.ForwardChain != nil || len(f.NAT) != 1) {
				t.Fatal("standalone service migration invented a filtering hook")
			}
		})
	}
}
