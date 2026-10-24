// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
)

type testPacket struct {
	in, out, source, destination, protocol string
	sport, dport, icmp                     int
}

// Evaluates only the ordered rule projection for new, valid packets. Native
// conntrack handling is independently verified by internal/network's netns gate.
func packetMatches(m network.PacketMatch, p testPacket) bool {
	src, dst := netip.MustParseAddr(p.source), netip.MustParseAddr(p.destination)
	if m.Family != 0 && (m.Family == 4) != src.Is4() || m.InInterface != "" && m.InInterface != p.in || m.OutInterface != "" && m.OutInterface != p.out {
		return false
	}
	if m.Protocol != "" && m.Protocol != p.protocol && !(m.Protocol == "ipv6-icmp" && p.protocol == "icmpv6") {
		return false
	}
	if m.Source != "" && !netip.MustParsePrefix(m.Source).Contains(src) || m.Destination != "" && !netip.MustParsePrefix(m.Destination).Contains(dst) {
		return false
	}
	for _, pair := range []struct {
		ranges []network.PortRange
		port   int
	}{{m.SourcePorts, p.sport}, {m.DestinationPorts, p.dport}} {
		if len(pair.ranges) == 0 {
			continue
		}
		found := false
		for _, r := range pair.ranges {
			end := r.End
			if end == 0 {
				end = r.Start
			}
			found = found || pair.port >= r.Start && pair.port <= end
		}
		if !found {
			return false
		}
	}
	if len(m.ICMPTypes) > 0 {
		found := false
		for _, kind := range m.ICMPTypes {
			found = found || kind == p.icmp
		}
		if !found {
			return false
		}
	}
	return true
}

func packetVerdict(chain *network.FirewallChain, p testPacket) string {
	for _, rule := range chain.Rules {
		if packetMatches(rule.Match, p) {
			return rule.Action
		}
	}
	return chain.Policy
}

func TestFirewallMigrationPreservesActualPacketDecisions(t *testing.T) {
	result, err := Compile(fixture(t, "firewall"))
	if err != nil {
		t.Fatal(err)
	}
	f := nativeProgram(t, result.Document).Network.Firewall
	if f == nil || f.Input == nil || f.ForwardChain == nil || f.DisableForward || f.Output != nil || f.Input.LogPrefix != "routerd firewall input deny " || f.ForwardChain.LogPrefix != "routerd firewall forward deny " {
		t.Fatal("filter hooks/default logging changed")
	}
	cases := []struct {
		name, chain, want string
		packet            testPacket
	}{
		{"wan-management", "input", "accept", testPacket{in: "eth0", source: "198.51.100.10", destination: "203.0.113.9", protocol: "tcp", dport: 22}},
		{"wan-management-other-source", "input", "drop", testPacket{in: "eth0", source: "192.0.2.10", destination: "203.0.113.9", protocol: "tcp", dport: 22}},
		{"loopback", "input", "accept", testPacket{in: "lo", source: "127.0.0.1", destination: "127.0.0.1", protocol: "tcp", dport: 4444}},
		{"unknown-interface", "input", "drop", testPacket{in: "eth9", source: "192.0.2.10", destination: "203.0.113.9", protocol: "udp", dport: 53}},
		{"untrust-dhcp-hole-precedes-denial", "input", "accept", testPacket{in: "eth0", source: "198.51.100.10", destination: "255.255.255.255", protocol: "udp", dport: 68}},
		{"trust-explicit-dhcp-denial-no-hole", "input", "drop", testPacket{in: "eth1", source: "192.0.2.10", destination: "255.255.255.255", protocol: "udp", dport: 68}},
		{"wan-icmp-echo", "input", "accept", testPacket{in: "eth0", source: "198.51.100.10", destination: "203.0.113.9", protocol: "icmp", icmp: 8}},
		{"wan-icmp-other", "input", "drop", testPacket{in: "eth0", source: "198.51.100.10", destination: "203.0.113.9", protocol: "icmp", icmp: 3}},
		{"ipip-means-ipv6-outer", "input", "accept", testPacket{in: "eth0", source: "2001:db8::1", destination: "2001:db8::2", protocol: "ipip"}},
		{"ipip-does-not-enable-v4-outer", "input", "drop", testPacket{in: "eth0", source: "198.51.100.10", destination: "203.0.113.9", protocol: "ipip"}},
		{"name-order-exception", "forward", "accept", testPacket{in: "eth1", out: "eth0", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", sport: 1234, dport: 80}},
		{"later-name-deny", "forward", "drop", testPacket{in: "eth1", out: "eth0", source: "192.0.2.43", destination: "203.0.113.11", protocol: "tcp", sport: 1234, dport: 80}},
		{"wrong-source-port", "forward", "drop", testPacket{in: "eth1", out: "eth0", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", sport: 1235, dport: 80}},
		{"trust-to-mgmt-drop", "forward", "drop", testPacket{in: "eth1", out: "eth2", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", dport: 443}},
		{"mgmt-to-trust-accept", "forward", "accept", testPacket{in: "eth2", out: "eth1", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", dport: 443}},
		{"same-role-untrust-accept", "forward", "accept", testPacket{in: "eth0", out: "eth3", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", dport: 443}},
		{"untrust-to-trust-drop", "forward", "drop", testPacket{in: "eth0", out: "eth1", source: "192.0.2.42", destination: "203.0.113.11", protocol: "tcp", dport: 443}},
		{"global-icmpv6-before-zone-deny", "forward", "accept", testPacket{in: "eth9", out: "eth1", source: "2001:db8::1", destination: "2001:db8::2", protocol: "icmpv6", icmp: 134}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := f.Input
			if tc.chain == "forward" {
				chain = f.ForwardChain
			}
			if got := packetVerdict(chain, tc.packet); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	if f.NAT[0].Type != "snat" || f.NAT[0].Address != "203.0.113.9" {
		t.Fatal("name-sorted SNAT priority or static address source was lost")
	}
	for _, tc := range []struct{ destination, source, want string }{
		{"198.51.100.12", "192.0.2.42", "snat"}, {"198.51.100.12", "192.0.2.43", "masquerade"},
		{"10.0.0.0", "192.0.2.43", ""}, {"10.255.255.255", "192.0.2.43", ""}, {"172.31.255.255", "192.0.2.43", ""}, {"192.168.0.0", "192.0.2.43", ""},
		{"9.255.255.255", "192.0.2.43", "masquerade"}, {"11.0.0.0", "192.0.2.43", "masquerade"}, {"172.32.0.0", "192.0.2.43", "masquerade"}, {"192.169.0.0", "192.0.2.43", "masquerade"},
		{"198.51.100.12", "198.18.0.1", ""},
	} {
		got := ""
		for _, n := range f.NAT {
			if packetMatches(n.Match, testPacket{out: "eth0", source: tc.source, destination: tc.destination, protocol: "tcp"}) {
				got = n.Type
				break
			}
		}
		if got != tc.want {
			t.Fatalf("NAT %s -> %s: %s, want %s", tc.source, tc.destination, got, tc.want)
		}
	}
}

func TestNATOnlyDoesNotInventFilterPolicy(t *testing.T) {
	d, err := Migrate(routerWith("    - apiVersion: net.routerd.net/v1alpha1\n      kind: NAT44Rule\n      metadata: {name: outbound}\n      spec: {type: masquerade, egressInterface: eth0, sourceRanges: [192.0.2.0/24]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := nativeProgram(t, d).Network.Firewall
	if f == nil || !f.DisableForward || f.Input != nil || f.Output != nil || f.ForwardChain != nil || len(f.NAT) != 1 {
		t.Fatal("NAT-only configuration acquired a filtering hook")
	}
}

func TestFirewallAmbiguitiesAndUnsupportedControlsFailAtomically(t *testing.T) {
	base := string(fixture(t, "firewall"))
	cases := []struct{ name, old, next, path, code string }{
		{"false-same-role", "sameRoleAccept: true", "sameRoleAccept: false", "sameRoleAccept", "unsupported_semantics"},
		{"rate-limit", "action: accept, log: true", "action: accept, log: true, rateLimit: {rate: 20}", "rateLimit", "unsupported_field"},
		{"conn-limit", "action: accept, log: true", "action: accept, log: true, connLimit: {maxPerSource: 2}", "connLimit", "unsupported_field"},
		{"dynamic-set", "protocol: tcp, port: 22", "protocol: tcp, port: 22, destinationSetRefs: [cloud]", "destinationSetRefs", "unsupported_field"},
		{"inactive-output-rule", "fromZone: z-wan, toZone: self", "fromZone: self, toZone: z-wan", "fromZone", "unsupported_semantics"},
		{"overlap-zones", "interfaces: [Interface/peer]", "interfaces: [Interface/wan]", "interfaces", "unsupported_semantics"},
		{"mixed-address-family", "srcCIDRs: [198.51.100.0/24]", "srcCIDRs: [198.51.100.0/24], destinationCIDRs: ['2001:db8::/32']", "srcCIDRs", "invalid_value"},
		{"dynamic-snat", "resource: IPv4StaticAddress/source", "resource: DHCPv4Client/wan-client", "snatAddressFrom", "unsupported_semantics"},
		{"nat-set", "type: masquerade", "type: masquerade\n        destinationSetRefs: [cloud]", "destinationSetRefs", "unsupported_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Migrate([]byte(strings.Replace(base, tc.old, tc.next, 1)))
			if !hasIssue(err, tc.path, tc.code) || !reflect.DeepEqual(d, config.Document{}) {
				t.Fatalf("missing atomic diagnostic: %v", err)
			}
		})
	}
	_, err := Migrate(routerWith("    - apiVersion: net.routerd.net/v1alpha1\n      kind: NAT44Rule\n      metadata: {name: rendered-only}\n      spec: {outboundInterface: eth0, sourceCIDRs: [192.0.2.0/24], translation: {type: interfaceAddress}}\n"))
	if !hasIssue(err, "translation", "unsupported_semantics") {
		t.Fatalf("renderer/controller disagreement was guessed: %v", err)
	}
	// Derived masquerade exists in the combined exporter but not the running
	// daemon's NAT controller, so explicit declaration must settle the intent.
	cut := strings.Index(base, "    - apiVersion: net.routerd.net/v1alpha1\n      kind: NAT44Rule")
	base = strings.Replace(base[:cut], "interface: wan, address: 203.0.113.9/32", "interface: lan, address: 192.0.2.1/24", 1)
	_, err = Migrate([]byte(base))
	if !hasIssue(err, "resources", "unsupported_semantics") || !strings.Contains(err.Error(), "derives trust-to-untrust") {
		t.Fatalf("implicit NAT disagreement was not diagnosed: %v", err)
	}
}

func TestIPv4ExclusionProjectionBoundariesAndLimit(t *testing.T) {
	original := netip.MustParsePrefix("192.0.2.0/24")
	excluded := []netip.Prefix{netip.MustParsePrefix("192.0.2.16/28"), netip.MustParsePrefix("192.0.2.64/27"), netip.MustParsePrefix("198.51.100.0/24")}
	set := []netip.Prefix{original}
	for _, e := range excluded {
		var exceeded bool
		set, exceeded = subtractIPv4Prefixes(set, e, 100)
		if exceeded {
			t.Fatal("unexpected overflow")
		}
	}
	for i := range 256 {
		a := netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
		want := !(i >= 16 && i < 32 || i >= 64 && i < 96)
		got, matches := false, 0
		for _, p := range set {
			if p.Contains(a) {
				got = true
				matches++
			}
		}
		if got != want || matches > 1 {
			t.Fatalf("address %s: result=%t matches=%d want=%t", a, got, matches, want)
		}
	}
	if got, exceeded := subtractIPv4Prefixes([]netip.Prefix{original}, netip.MustParsePrefix("0.0.0.0/0"), 1); exceeded || len(got) != 0 {
		t.Fatal("superset exclusion must yield no destination")
	}
	if _, exceeded := subtractIPv4Prefixes([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, netip.MustParsePrefix("192.0.2.1/32"), 1); !exceeded {
		t.Fatal("expansion limit was not enforced")
	}
}
