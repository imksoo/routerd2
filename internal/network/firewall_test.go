//go:build linux

package network

import (
	"context"
	"strings"
	"testing"
)

func TestNATConnectionMarksRequireDistinctSourcePaths(t *testing.T) {
	primary := NATRule{Type: "snat", Match: PacketMatch{Family: 4, Source: "198.18.0.0/24", OutInterface: "wan0"}, Address: "192.0.2.1", ConnectionMark: 41}
	backup := NATRule{Type: "snat", Match: PacketMatch{Family: 4, Source: "198.18.0.0/24", OutInterface: "wan1"}, Address: "192.0.2.2", ConnectionMark: 42}
	if err := validateFirewall(&Firewall{NAT: []NATRule{primary, backup}}); err != nil {
		t.Fatal(err)
	}
	backup.ConnectionMark = primary.ConnectionMark
	if err := validateFirewall(&Firewall{NAT: []NATRule{primary, backup}}); err == nil {
		t.Fatal("two WANs accepted the same connection mark")
	}
	primary.Match.Source = ""
	if err := validateFirewall(&Firewall{NAT: []NATRule{primary}}); err == nil {
		t.Fatal("connection mark accepted without a forwarded source prefix")
	}
}

func TestFirewallValidationRejectsAmbiguousHooksAndTranslations(t *testing.T) {
	bad := []*Firewall{
		{DisableForward: true, Forward: []Forward{{From: "lan0", To: "wan0"}}},
		{DisableForward: true, ForwardChain: &FirewallChain{}},
		{Input: &FirewallChain{Policy: "reject"}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{OutInterface: "wan0"}, Action: "accept"}}}},
		{Output: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{InInterface: "lan0"}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Family: 4, Destination: "2001:db8::/64"}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Protocol: "udp", ICMPTypes: []int{8}}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Family: 6, Protocol: "icmp"}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{DestinationPorts: []PortRange{{Start: 53}}}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Protocol: "tcp", DestinationPorts: []PortRange{{Start: 80, End: 79}}}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Protocol: "tcp", DestinationPorts: []PortRange{{Start: 80}, {Start: 80, End: 80}}}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Protocol: "icmpv6", ICMPTypes: []int{256}}, Action: "accept"}}}},
		{Input: &FirewallChain{Rules: []FirewallRule{{Action: "continue"}}}},
		{Input: &FirewallChain{LogPrefix: "prefix\nforged"}},
		{Input: &FirewallChain{LogPrefix: strings.Repeat("x", 128)}},
		{DisableForward: true, NAT: []NATRule{{Type: "snat", Match: PacketMatch{OutInterface: "wan0"}, Address: "198.51.100.1"}}},
		{DisableForward: true, NAT: []NATRule{{Type: "snat", Match: PacketMatch{Family: 4, OutInterface: "wan0"}, Address: "2001:db8::1"}}},
		{DisableForward: true, NAT: []NATRule{{Type: "dnat", Match: PacketMatch{Family: 4, OutInterface: "wan0"}, Address: "192.0.2.1"}}},
		{DisableForward: true, NAT: []NATRule{{Type: "masquerade", Match: PacketMatch{Family: 4, OutInterface: "wan0"}, Address: "198.51.100.1"}}},
		{DisableForward: true, NAT: []NATRule{{Type: "snat", Match: PacketMatch{Family: 4, OutInterface: "wan0"}, Address: "198.51.100.1", Ports: &PortRange{Start: 80}}}},
	}
	for i, f := range bad {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			if _, err := e.Apply(context.Background(), 1, Desired{Firewall: f}); err == nil {
				t.Fatalf("accepted invalid firewall: %+v", f)
			}
			if len(k.writes) > 0 {
				t.Fatalf("validation mutated kernel: %v", k.writes)
			}
		})
	}
}

func TestFirewallNewAPIJournalRestartAndNATOnly(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	f := &Firewall{DisableForward: true, Input: &FirewallChain{Rules: []FirewallRule{{Match: PacketMatch{Protocol: "ipv6-icmp", ICMPTypes: []int{135, 136}}, Action: "accept"}}}, NAT: []NATRule{{Type: "dnat", Match: PacketMatch{Family: 4, InInterface: "wan0", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 8443}}}, Address: "192.0.2.2", Ports: &PortRange{Start: 443}}}}
	d := Desired{Firewall: f}
	s, err := e.Apply(context.Background(), 1, d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Desired.Firewall.Input.Policy != "drop" || s.Desired.Firewall.Input.Rules[0].Match.Family != 6 || s.Desired.Firewall.Input.Rules[0].Match.Protocol != "icmpv6" {
		t.Fatal("firewall family/default policy normalization failed")
	}
	if f.Input.Policy != "" || f.Input.Rules[0].Match.Protocol != "ipv6-icmp" || f.NAT[0].Ports.End != 0 {
		t.Fatal("firewall normalization mutated caller")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if k.firewall.Present {
		t.Fatal("owned firewall remains after removal")
	}
}

func TestUnownedFirewallCollisionFailsBeforeLinkOrAddressMutation(t *testing.T) {
	for _, marker := range []bool{false, true} {
		k := fake()
		k.firewall = FirewallState{Table: FirewallTable, Present: true, Owned: marker}
		e, _ := engineFor(t, k)
		if _, err := e.Apply(context.Background(), 1, exampleDesired()); err == nil {
			t.Fatal("adopted a table without a journal")
		}
		if len(k.writes) != 0 {
			t.Fatalf("mutated networking before reporting firewall ownership conflict: %v", k.writes)
		}
	}
}
