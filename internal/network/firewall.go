package network

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// FirewallChain first drops invalid conntrack state, then accepts established
// and related flows, then evaluates Rules in order. The forward chain next
// evaluates legacy Forward pairs, then applies Policy. Existing connections
// survive table replacements and a more restrictive policy for new flows.
// Policy is accept/drop; empty means drop. LogPrefix logs packets reaching the
// terminal policy through the kernel logger; empty disables terminal logging.
// Loopback and ICMPv6 are never implicitly accepted: declare those rules when
// enabling an input/output policy. The legacy compiler preserves its own
// historical defaults explicitly in its rule projection.
type FirewallChain struct {
	Policy    string         `json:"policy,omitempty"`
	Rules     []FirewallRule `json:"rules,omitempty"`
	LogPrefix string         `json:"log_prefix,omitempty"`
}

type FirewallRule struct {
	Match     PacketMatch `json:"match,omitempty"`
	Action    string      `json:"action"`
	LogPrefix string      `json:"log_prefix,omitempty"`
}

// PacketMatch fields are ANDed. Port ranges and ICMP types are alternatives
// within their field. Family zero matches both families unless a prefix or
// ICMP protocol narrows it. Protocol is empty/tcp/udp/icmp/icmpv6/ipip/vrrp.
// The native L4 matcher handles IPv6 extension headers; it does not assume the
// transport header immediately follows the fixed IPv6 header.
type PacketMatch struct {
	Family           int         `json:"family,omitempty"`
	Source           string      `json:"source,omitempty"`
	Destination      string      `json:"destination,omitempty"`
	InInterface      string      `json:"in_interface,omitempty"`
	OutInterface     string      `json:"out_interface,omitempty"`
	Protocol         string      `json:"protocol,omitempty"`
	SourcePorts      []PortRange `json:"source_ports,omitempty"`
	DestinationPorts []PortRange `json:"destination_ports,omitempty"`
	ICMPTypes        []int       `json:"icmp_types,omitempty"`
}

// PortRange is inclusive. End zero means a single port equal to Start.
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end,omitempty"`
}

// NATRule translates new connections in the routerd2 inet table. Type is
// dnat/snat/masquerade. DNAT runs at prerouting; SNAT and masquerade run at
// postrouting, in the order declared before legacy Masquerade entries. It does
// not implicitly authorize forwarding. Match.Family must explicitly be 4/6;
// Address is a same-family unicast IP for DNAT/SNAT and empty for masquerade.
// Ports optionally selects a translated TCP/UDP port or inclusive port range.
// Existing conntrack mappings survive a ruleset replacement.
type NATRule struct {
	Type    string      `json:"type"`
	Match   PacketMatch `json:"match"`
	Address string      `json:"address,omitempty"`
	Ports   *PortRange  `json:"ports,omitempty"`
}

func normalizedPortRange(p PortRange) PortRange {
	if p.End == 0 {
		p.End = p.Start
	}
	return p
}

func validPortRange(p PortRange) bool {
	p = normalizedPortRange(p)
	return p.Start >= 1 && p.Start <= p.End && p.End <= 65535
}

func matchFamily(m PacketMatch) int {
	if m.Family != 0 {
		return m.Family
	}
	for _, cidr := range []string{m.Source, m.Destination} {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			if p.Addr().Is4() {
				return 4
			}
			return 6
		}
	}
	switch m.Protocol {
	case "icmp":
		return 4
	case "icmpv6", "ipv6-icmp":
		return 6
	}
	return 0
}

func validatePacketMatch(m PacketMatch, hook string) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if m.Family != 0 && m.Family != 4 && m.Family != 6 {
		add("family must be 0, 4 or 6")
	}
	family := m.Family
	for _, cidr := range []string{m.Source, m.Destination} {
		if cidr == "" {
			continue
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil || p.Addr().Is4In6() || p != p.Masked() {
			add("invalid source/destination network %q", cidr)
			continue
		}
		current := 6
		if p.Addr().Is4() {
			current = 4
		}
		if family != 0 && family != current {
			add("source, destination and family must agree")
		}
		family = current
	}
	for _, name := range []string{m.InInterface, m.OutInterface} {
		if name != "" && !validName(name) {
			add("invalid match interface %q", name)
		}
	}
	if hook == "input" && m.OutInterface != "" {
		add("input rules cannot match an output interface")
	}
	if hook == "output" && m.InInterface != "" {
		add("output rules cannot match an input interface")
	}
	if hook == "prerouting" && m.OutInterface != "" {
		add("prerouting DNAT cannot match an output interface")
	}
	switch m.Protocol {
	case "", "tcp", "udp", "ipip", "vrrp":
	case "icmp":
		if family != 0 && family != 4 {
			add("ICMP requires IPv4")
		}
	case "icmpv6", "ipv6-icmp":
		if family != 0 && family != 6 {
			add("ICMPv6 requires IPv6")
		}
	default:
		add("unsupported protocol %q", m.Protocol)
	}
	if len(m.SourcePorts)+len(m.DestinationPorts) > 0 && m.Protocol != "tcp" && m.Protocol != "udp" {
		add("port matches require tcp or udp")
	}
	for _, ranges := range [][]PortRange{m.SourcePorts, m.DestinationPorts} {
		seen := map[PortRange]bool{}
		for _, p := range ranges {
			if !validPortRange(p) {
				add("port range must be within 1..65535 with start <= end")
			}
			p = normalizedPortRange(p)
			if seen[p] {
				add("duplicate port range %d-%d", p.Start, p.End)
			}
			seen[p] = true
		}
	}
	if len(m.ICMPTypes) > 0 && m.Protocol != "icmp" && m.Protocol != "icmpv6" && m.Protocol != "ipv6-icmp" {
		add("ICMP type matches require icmp or icmpv6")
	}
	seenTypes := map[int]bool{}
	for _, kind := range m.ICMPTypes {
		if kind < 0 || kind > 255 || seenTypes[kind] {
			add("ICMP types must be unique values in 0..255")
		}
		seenTypes[kind] = true
	}
	// Each set of alternatives becomes explicit native rules. Bound expansion
	// before constructing a transaction rather than truncating policy silently.
	if max(1, len(m.SourcePorts))*max(1, len(m.DestinationPorts))*max(1, len(m.ICMPTypes)) > 4096 {
		add("a match may expand to at most 4096 port/type combinations")
	}
	return errors.Join(errs...)
}

func validLogPrefix(prefix string) bool {
	return len(prefix) <= 127 && !strings.ContainsAny(prefix, "\x00\n\r")
}

func validateFirewall(f *Firewall) error {
	if f == nil {
		return nil
	}
	var errs []error
	if f.DisableForward && (f.ForwardChain != nil || len(f.Forward) > 0) {
		errs = append(errs, errors.New("firewall: disable_forward conflicts with forward_chain/forward pairs"))
	}
	for _, item := range []struct {
		name  string
		chain *FirewallChain
	}{{"input", f.Input}, {"output", f.Output}, {"forward", f.ForwardChain}} {
		if item.chain == nil {
			continue
		}
		chain := item.chain
		if chain.Policy != "" && chain.Policy != "accept" && chain.Policy != "drop" {
			errs = append(errs, fmt.Errorf("firewall %s: policy must be accept/drop", item.name))
		}
		if !validLogPrefix(chain.LogPrefix) {
			errs = append(errs, fmt.Errorf("firewall %s: invalid log prefix", item.name))
		}
		for i, rule := range chain.Rules {
			if err := validatePacketMatch(rule.Match, item.name); err != nil {
				errs = append(errs, fmt.Errorf("firewall %s rule %d: %w", item.name, i, err))
			}
			if rule.Action != "accept" && rule.Action != "drop" && rule.Action != "reject" {
				errs = append(errs, fmt.Errorf("firewall %s rule %d: action must be accept/drop/reject", item.name, i))
			}
			if !validLogPrefix(rule.LogPrefix) {
				errs = append(errs, fmt.Errorf("firewall %s rule %d: invalid log prefix", item.name, i))
			}
		}
	}
	for i, rule := range f.NAT {
		hook := "postrouting"
		if rule.Type == "dnat" {
			hook = "prerouting"
		}
		if err := validatePacketMatch(rule.Match, hook); err != nil {
			errs = append(errs, fmt.Errorf("NAT rule %d: %w", i, err))
		}
		if rule.Match.Family != 4 && rule.Match.Family != 6 {
			errs = append(errs, fmt.Errorf("NAT rule %d: explicit family 4/6 is required", i))
		}
		switch rule.Type {
		case "dnat", "snat":
			a, err := netip.ParseAddr(rule.Address)
			if err != nil || a.Is4In6() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() || a.Is4() != (rule.Match.Family == 4) {
				errs = append(errs, fmt.Errorf("NAT rule %d: address must be a same-family unicast IP", i))
			}
		case "masquerade":
			if rule.Address != "" {
				errs = append(errs, fmt.Errorf("NAT rule %d: masquerade cannot specify an address", i))
			}
		default:
			errs = append(errs, fmt.Errorf("NAT rule %d: type must be dnat/snat/masquerade", i))
		}
		if rule.Type != "dnat" && rule.Match.OutInterface == "" {
			errs = append(errs, fmt.Errorf("NAT rule %d: source NAT requires an explicit output interface", i))
		}
		if rule.Ports != nil && (!validPortRange(*rule.Ports) || rule.Match.Protocol != "tcp" && rule.Match.Protocol != "udp") {
			errs = append(errs, fmt.Errorf("NAT rule %d: translated ports require a valid range and tcp/udp", i))
		}
	}
	return errors.Join(errs...)
}

func normalizePacketMatch(m PacketMatch) PacketMatch {
	m.Family = matchFamily(m)
	if m.Protocol == "ipv6-icmp" {
		m.Protocol = "icmpv6"
	}
	if p, err := netip.ParsePrefix(m.Source); err == nil {
		m.Source = p.Masked().String()
	}
	if p, err := netip.ParsePrefix(m.Destination); err == nil {
		m.Destination = p.Masked().String()
	}
	for i, p := range m.SourcePorts {
		m.SourcePorts[i] = normalizedPortRange(p)
	}
	for i, p := range m.DestinationPorts {
		m.DestinationPorts[i] = normalizedPortRange(p)
	}
	return m
}

func normalizeFirewall(f *Firewall) {
	if f == nil {
		return
	}
	for _, chain := range []*FirewallChain{f.Input, f.Output, f.ForwardChain} {
		if chain == nil {
			continue
		}
		if chain.Policy == "" {
			chain.Policy = "drop"
		}
		for i := range chain.Rules {
			chain.Rules[i].Match = normalizePacketMatch(chain.Rules[i].Match)
		}
	}
	for i := range f.NAT {
		r := &f.NAT[i]
		r.Match = normalizePacketMatch(r.Match)
		if a, err := netip.ParseAddr(r.Address); err == nil {
			r.Address = a.String()
		}
		if r.Ports != nil {
			p := normalizedPortRange(*r.Ports)
			r.Ports = &p
		}
	}
}

func firewallInterfaces(f *Firewall) []string {
	if f == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name != "" && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	for _, p := range f.Forward {
		add(p.From)
		add(p.To)
	}
	for _, n := range f.Masquerade {
		add(n.OutInterface)
	}
	for _, chain := range []*FirewallChain{f.Input, f.Output, f.ForwardChain} {
		if chain != nil {
			for _, rule := range chain.Rules {
				add(rule.Match.InInterface)
				add(rule.Match.OutInterface)
			}
		}
	}
	for _, n := range f.NAT {
		add(n.Match.InInterface)
		add(n.Match.OutInterface)
	}
	return names
}
