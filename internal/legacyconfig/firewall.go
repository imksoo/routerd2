// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// The projection flattens the old terminal zone-pair chains. It retains the
// original order (resource name, not YAML order) and bounds Cartesian expansion
// before building a native netlink transaction. No legacy controller is loaded.
const maxFirewallProjection = 8192

type migrationZone struct {
	resource resource
	role     string
	ifnames  []string
}

type migrationACL struct {
	resource resource
	from, to string
	rules    []network.FirewallRule
}

func (c *compiler) resourcesOfKind(kind string) []resource {
	var out []resource
	for _, r := range c.resources {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (c *compiler) firewall(vrrpNames map[string]string) []resourceprogram.Resource {
	zrs, prs, rrs, nrs := c.resourcesOfKind("FirewallZone"), c.resourcesOfKind("FirewallPolicy"), c.resourcesOfKind("FirewallRule"), c.resourcesOfKind("NAT44Rule")
	if len(zrs)+len(prs)+len(rrs)+len(nrs) == 0 {
		return nil
	}
	f := &network.Firewall{DisableForward: true}
	c.document.Network.Firewall = f
	logDeny := false
	for _, r := range prs {
		c.allow(r, "logDeny", "sameRoleAccept")
		s := r.Spec.(api.FirewallPolicySpec)
		logDeny = logDeny || s.LogDeny
		if child(r.node, "sameRoleAccept") != nil && !s.SameRoleAccept {
			c.reject(r, "sameRoleAccept", "unsupported_semantics", "the pinned Linux renderer always enables same-role accept even when false; declare true or express the intended denial with explicit ordered rules")
		}
		if len(zrs) == 0 {
			c.reject(r, "", "unsupported_semantics", "policy-only configuration has different hook behavior in the legacy combined renderer and running firewall controller; explicit zones are required")
		}
		c.target(r, "network.firewall")
	}
	var zones []migrationZone
	zoneNames, zoneByRef, ifOwner := map[string]bool{}, map[string]string{}, map[string]string{}
	interfaces := 0
	for _, r := range zrs {
		c.allow(r, "role", "interfaces")
		s := r.Spec.(api.FirewallZoneSpec)
		if r.Metadata.Name == "self" || s.Role != "mgmt" && s.Role != "trust" && s.Role != "untrust" {
			c.reject(r, "role", "invalid_value", "zone must have role mgmt, trust, or untrust and cannot use the reserved name self")
		}
		if len(s.Interfaces) == 0 {
			c.reject(r, "interfaces", "required", "zone requires at least one explicit interface")
		}
		zoneNames[r.Metadata.Name] = true
		z := migrationZone{resource: r, role: s.Role}
		seen := map[string]bool{}
		for _, ref := range s.Interfaces {
			name := c.filterInterface(r, "interfaces", ref)
			if name == "" {
				continue
			}
			names := []string{name}
			for _, service := range c.document.Services {
				if service.VRRP != nil && service.VRRP.Interface == name {
					names = append(names, service.VRRP.VIPInterface)
				}
			}
			zoneByRef[legacyRefName(ref)] = r.Metadata.Name
			for _, name := range names {
				if seen[name] {
					continue
				}
				seen[name] = true
				if previous := ifOwner[name]; previous != "" && previous != r.Metadata.Name {
					c.reject(r, "interfaces", "unsupported_semantics", "an interface in multiple zones has order-dependent legacy service holes; split the overlapping zone membership before migration")
				}
				ifOwner[name] = r.Metadata.Name
				z.ifnames = append(z.ifnames, name)
			}
		}
		sort.Strings(z.ifnames)
		interfaces += len(z.ifnames)
		zones = append(zones, z)
		c.target(r, "network.firewall.input")
		c.target(r, "network.firewall.forward_chain")
		c.note(r, "Preserves legacy loopback/ICMPv6 acceptance, INPUT/FORWARD default drop, conntrack precedence, name-sorted ACLs and terminal role policy; no OUTPUT hook is added.")
	}
	if len(zones) > 64 || interfaces > 90 || interfaces*interfaces+interfaces > maxFirewallProjection {
		c.issue(nil, "$.spec.resources", "resource_limit", "zone/interface projection exceeds the bounded native firewall size")
		return nil
	}
	var acls []migrationACL
	for _, r := range rrs {
		s := r.Spec.(api.FirewallRuleSpec)
		if s.FromZone == "self" {
			c.reject(r, "fromZone", "unsupported_semantics", "legacy Linux has no OUTPUT or self-originating pair chain; migration cannot activate an otherwise inactive rule")
		} else if !zoneNames[s.FromZone] {
			c.reject(r, "fromZone", "reference", "source zone is missing")
		}
		if s.ToZone != "self" && !zoneNames[s.ToZone] {
			c.reject(r, "toZone", "reference", "destination zone is missing")
		}
		acls = append(acls, migrationACL{resource: r, from: s.FromZone, to: s.ToZone, rules: c.acl(r)})
		c.target(r, "network.firewall")
	}
	if len(zones) > 0 || len(acls) > 0 {
		f.DisableForward = false
		f.Input = &network.FirewallChain{Policy: "drop", Rules: []network.FirewallRule{
			{Match: network.PacketMatch{InInterface: "lo"}, Action: "accept"},
			{Match: network.PacketMatch{Family: 6, Protocol: "icmpv6"}, Action: "accept"},
		}}
		f.ForwardChain = &network.FirewallChain{Policy: "drop", Rules: []network.FirewallRule{
			{Match: network.PacketMatch{Family: 6, Protocol: "icmpv6"}, Action: "accept"},
		}}
		if logDeny {
			f.Input.LogPrefix, f.ForwardChain.LogPrefix = "routerd firewall input deny ", "routerd firewall forward deny "
		}
		count := 3
		appendRule := func(chain *network.FirewallChain, rule network.FirewallRule) bool {
			count += max(1, len(rule.Match.SourcePorts)) * max(1, len(rule.Match.DestinationPorts)) * max(1, len(rule.Match.ICMPTypes))
			if count > maxFirewallProjection {
				c.issue(nil, "$.spec.resources", "resource_limit", "firewall expansion exceeds 8192 native packet rules")
				return false
			}
			chain.Rules = append(chain.Rules, rule)
			return true
		}
		for _, from := range zones {
			// Internal DHCP exceptions are scoped to the old logical resource
			// mapping. Trust/mgmt deliberately have no hole before their ACLs.
			if from.role == "untrust" {
				for _, r := range c.resources {
					ref, port := "", 0
					switch s := r.Spec.(type) {
					case api.DHCPv4ClientSpec:
						ref, port = s.Interface, 68
					case api.DHCPv6PrefixDelegationSpec:
						ref, port = s.Interface, 546
					}
					if port > 0 && zoneByRef[legacyRefName(ref)] == from.resource.Metadata.Name {
						name := c.filterInterface(r, "interface", ref)
						if name != "" && !appendRule(f.Input, network.FirewallRule{Match: network.PacketMatch{InInterface: name, Protocol: "udp", DestinationPorts: []network.PortRange{{Start: port}}}, Action: "accept"}) {
							return nil
						}
					}
				}
			}
			for _, in := range from.ifnames {
				if !c.firewallPair(from, migrationZone{resource: resource{Resource: api.Resource{Metadata: api.ObjectMeta{Name: "self"}}}, role: "self"}, in, "", acls, logDeny, f.Input, appendRule) {
					return nil
				}
			}
			for _, to := range zones {
				for _, in := range from.ifnames {
					for _, out := range to.ifnames {
						if !c.firewallPair(from, to, in, out, acls, logDeny, f.ForwardChain, appendRule) {
							return nil
						}
					}
				}
			}
		}
	}
	if len(nrs) == 0 && c.rendererWouldDeriveNAT(zrs) {
		c.issue(nil, "$.spec.resources", "unsupported_semantics", "legacy combined renderer derives trust-to-untrust masquerade here, while the running Linux NAT controller does not; declare explicit NAT44Rule resources to resolve this difference")
	}
	var translations []resourceprogram.Resource
	natRules := 0
	for _, r := range nrs {
		translation := c.nat44(r, vrrpNames)
		natRules += len(translation.NAT)
		if natRules > maxFirewallProjection {
			c.reject(r, "", "resource_limit", "NAT expansion exceeds 8192 native packet rules")
			return nil
		}
		if len(translation.NAT) > 0 {
			translations = append(translations, translation)
		}
	}
	return translations
}

func (c *compiler) firewallPair(from, to migrationZone, in, out string, rules []migrationACL, logDeny bool, chain *network.FirewallChain, appendRule func(*network.FirewallChain, network.FirewallRule) bool) bool {
	for _, acl := range rules {
		if acl.from != from.resource.Metadata.Name || acl.to != to.resource.Metadata.Name {
			continue
		}
		for _, rule := range acl.rules {
			rule.Match.InInterface, rule.Match.OutInterface = in, out
			if !appendRule(chain, rule) {
				return false
			}
		}
	}
	action := "drop"
	if to.role == "self" {
		if from.role == "mgmt" || from.role == "trust" {
			action = "accept"
		}
	} else if from.role == to.role || from.role == "mgmt" || from.role == "trust" && to.role != "mgmt" {
		action = "accept"
	}
	rule := network.FirewallRule{Match: network.PacketMatch{InInterface: in, OutInterface: out}, Action: action}
	if action == "drop" && logDeny {
		rule.LogPrefix = "routerd firewall " + from.resource.Metadata.Name + "-to-" + to.resource.Metadata.Name + " deny "
	}
	return appendRule(chain, rule)
}

func legacyRefName(value string) string {
	value = strings.TrimSpace(value)
	if _, name, ok := strings.Cut(value, "/"); ok {
		return strings.TrimSpace(name)
	}
	return value
}

// The original filter and NAT controllers used different alias builders. Keep
// their differences visible rather than allowing the migration to fix a wrong
// interface name by silently changing packet policy.
func (c *compiler) filterInterface(r resource, field, ref string) string {
	// nftFirewallZones uses the exact alias (or the suffix after '/'). It
	// does not trim that suffix: accepting a normalized name here could turn
	// an invalid old zone into an active filtering or service-exception rule.
	name := ref
	if _, suffix, qualified := strings.Cut(ref, "/"); qualified {
		name = suffix
	}
	owner := c.byRef[c.aliasOwner[name]]
	if owner == nil {
		// aliasOwner stores the full API resource ID; resolve by kind/name too.
		for i := range c.resources {
			if c.resources[i].ID() == c.aliasOwner[name] {
				owner = &c.resources[i]
				break
			}
		}
	}
	if owner == nil {
		c.reject(r, field, "reference", "filter interface must resolve to an original supported link resource")
		return ""
	}
	switch s := owner.Spec.(type) {
	case api.InterfaceSpec:
		return s.IfName
	case api.BridgeSpec:
		return first(s.IfName, owner.Metadata.Name)
	case api.WireGuardInterfaceSpec:
		return first(s.IfName, owner.Metadata.Name)
	case api.DSLiteTunnelSpec:
		old := first(s.TunnelName, owner.Metadata.Name)
		if old != c.aliases[owner.Metadata.Name] {
			c.reject(r, field, "unsupported_semantics", "legacy DS-Lite filter/NAT alias differs from the tunnel controller's default name; specify tunnelName explicitly")
			return ""
		}
		return old
	default:
		c.reject(r, field, "unsupported_semantics", "this link kind is absent from the pinned Linux filter alias builder")
		return ""
	}
}

func (c *compiler) acl(r resource) []network.FirewallRule {
	c.allow(r, "fromZone", "toZone", "srcCIDRs", "destinationCIDRs", "protocol", "port", "sourcePorts", "destinationPorts", "icmpType", "icmp-type", "icmpv6Type", "icmpv6-type", "action", "log")
	s := r.Spec.(api.FirewallRuleSpec)
	base := network.PacketMatch{Protocol: s.Protocol}
	switch s.Protocol {
	case "", "tcp", "udp":
	case "icmp":
		base.Family = 4
	case "icmpv6", "ipv6-icmp", "ipip":
		base.Family = 6
	default:
		c.reject(r, "protocol", "invalid_value", "protocol is outside the preserved legacy ACL subset")
	}
	if s.Action != "accept" && s.Action != "drop" && s.Action != "reject" {
		c.reject(r, "action", "invalid_value", "action must be accept, drop, or reject")
	}
	base.SourcePorts = c.ports(r, "sourcePorts", s.SourcePorts)
	base.DestinationPorts = c.ports(r, "destinationPorts", s.DestinationPorts)
	if s.Port != 0 {
		if len(s.DestinationPorts) != 0 {
			c.reject(r, "port", "conflict", "port and destinationPorts cannot be combined")
		}
		base.DestinationPorts = append(base.DestinationPorts, c.ports(r, "port", []api.FirewallPort{api.FirewallPort(strconv.Itoa(s.Port))})...)
	}
	if len(base.SourcePorts)+len(base.DestinationPorts) > 0 && s.Protocol != "tcp" && s.Protocol != "udp" {
		c.reject(r, "protocol", "invalid_value", "port matches require tcp or udp")
	}
	if max(1, len(base.SourcePorts))*max(1, len(base.DestinationPorts)) > 4096 {
		c.reject(r, "destinationPorts", "resource_limit", "source/destination port expansion exceeds 4096 combinations")
		return nil
	}
	for _, item := range []struct {
		a, b, field string
		family      int
	}{
		{s.ICMPType, s.ICMPTypeKebab, "icmpType", 4}, {s.ICMPv6Type, s.ICMPv6TypeKebab, "icmpv6Type", 6},
	} {
		if item.a == "" && item.b == "" {
			continue
		}
		if item.a != "" && item.b != "" && item.a != item.b {
			c.reject(r, item.field, "conflict", "camelCase and kebab-case type selectors disagree")
		}
		if item.family == 4 && s.Protocol != "icmp" || item.family == 6 && s.Protocol != "icmpv6" && s.Protocol != "ipv6-icmp" {
			c.reject(r, item.field, "invalid_value", "ICMP type selector must agree with the protocol family")
		}
		n, ok := icmpNumber(first(item.a, item.b), item.family)
		if !ok {
			c.reject(r, item.field, "invalid_value", "ICMP type must be an integer in 0..255 or a supported legacy type name")
		} else {
			base.ICMPTypes = []int{n}
		}
	}
	sources := c.packetCIDRs(r, "srcCIDRs", s.SourceCIDRs, 0, true)
	destinations := c.packetCIDRs(r, "destinationCIDRs", s.DestinationCIDRs, 0, true)
	if len(sources)*len(destinations) > maxFirewallProjection {
		c.reject(r, "destinationCIDRs", "resource_limit", "source/destination CIDR expansion exceeds 8192 combinations")
		return nil
	}
	var out []network.FirewallRule
	for _, source := range sources {
		for _, destination := range destinations {
			m := base
			compatible := true
			for _, prefix := range []netip.Prefix{source, destination} {
				if !prefix.IsValid() {
					continue
				}
				family := 6
				if prefix.Addr().Is4() {
					family = 4
				}
				if m.Family != 0 && m.Family != family {
					compatible = false
				}
				m.Family = family
			}
			if !compatible {
				continue
			}
			if source.IsValid() {
				m.Source = source.String()
			}
			if destination.IsValid() {
				m.Destination = destination.String()
			}
			rule := network.FirewallRule{Match: m, Action: s.Action}
			if s.Log {
				rule.LogPrefix = "routerd firewall " + r.Metadata.Name + " "
			}
			out = append(out, rule)
		}
	}
	if len(out) == 0 {
		c.reject(r, "srcCIDRs", "invalid_value", "no compatible source/destination/protocol address-family combination remains")
	}
	return out
}

func (c *compiler) ports(r resource, field string, ports []api.FirewallPort) []network.PortRange {
	var out []network.PortRange
	seen := map[network.PortRange]bool{}
	for _, value := range ports {
		left, right, ranged := strings.Cut(strings.TrimSpace(string(value)), "-")
		start, err := strconv.Atoi(left)
		end := start
		if ranged {
			var e error
			end, e = strconv.Atoi(right)
			if e != nil {
				err = e
			}
		}
		if err != nil || start < 1 || end < start || end > 65535 {
			c.reject(r, field, "invalid_value", "port must be 1..65535 or an ordered inclusive range")
			continue
		}
		p := network.PortRange{Start: start, End: end}
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

func icmpNumber(value string, family int) (int, bool) {
	value = strings.TrimSpace(value)
	if n, err := strconv.Atoi(value); err == nil {
		return n, n >= 0 && n <= 255
	}
	names := map[string]int{"echo-reply": 0, "destination-unreachable": 3, "source-quench": 4, "redirect": 5, "echo-request": 8, "router-advertisement": 9, "router-solicitation": 10, "time-exceeded": 11, "parameter-problem": 12, "timestamp-request": 13, "timestamp-reply": 14}
	if family == 6 {
		names = map[string]int{"destination-unreachable": 1, "packet-too-big": 2, "time-exceeded": 3, "parameter-problem": 4, "echo-request": 128, "echo-reply": 129, "router-solicit": 133, "nd-router-solicit": 133, "router-advert": 134, "nd-router-advert": 134, "neighbor-solicit": 135, "nd-neighbor-solicit": 135, "neighbor-advert": 136, "nd-neighbor-advert": 136}
	}
	n, ok := names[value]
	return n, ok
}

func (c *compiler) packetCIDRs(r resource, field string, values []string, family int, wildcard bool) []netip.Prefix {
	if len(values) > 1024 {
		c.reject(r, field, "resource_limit", "a packet selector may contain at most 1024 CIDRs")
		return nil
	}
	if len(values) == 0 && wildcard {
		return []netip.Prefix{{}}
	}
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, value := range values {
		p, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil || p.Addr().Is4In6() || family == 4 && !p.Addr().Is4() || family == 6 && !p.Addr().Is6() {
			c.reject(r, field, "invalid_value", "CIDR must have the required address family and cannot be an IPv4-mapped IPv6 prefix")
			continue
		}
		p = p.Masked()
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

func (c *compiler) rendererWouldDeriveNAT(zones []resource) bool {
	trust, untrust := map[string]bool{}, false
	for _, r := range zones {
		s := r.Spec.(api.FirewallZoneSpec)
		for _, ref := range s.Interfaces {
			if s.Role == "trust" {
				trust[legacyRefName(ref)] = true
			}
			if s.Role == "untrust" {
				untrust = true
			}
		}
	}
	if !untrust {
		return false
	}
	for _, r := range c.resources {
		if s, ok := r.Spec.(api.IPv4StaticAddressSpec); ok && trust[s.Interface] {
			if p, err := netip.ParsePrefix(s.Address); err == nil && p.Addr().Is4() {
				return true
			}
		}
	}
	return false
}

func (c *compiler) nat44(r resource, elections map[string]string) resourceprogram.Resource {
	c.allow(r, "type", "egressInterface", "sourceRanges", "destinationCIDRs", "excludeDestinationCIDRs", "snatAddress", "snatAddressFrom", "outboundInterface", "sourceCIDRs", "translation", "when")
	s := r.Spec.(api.NAT44RuleSpec)
	out := resourceprogram.Resource{ID: "nat/" + r.Metadata.Name, When: c.when(r, s.When, elections)}
	if meaningful(child(r.node, "translation")) || s.OutboundInterface != "" || len(s.SourceCIDRs) > 0 {
		c.reject(r, "translation", "unsupported_semantics", "legacy source-NAT translation form is rendered by the combined exporter but not reconciled by the pinned running Linux NAT controller; rewrite its intended semantics as explicit type/egressInterface/sourceRanges")
		return resourceprogram.Resource{}
	}
	if s.Type != "masquerade" && s.Type != "snat" {
		c.reject(r, "type", "invalid_value", "NAT type must be masquerade or snat")
	}
	if s.EgressInterface == "" || strings.Contains(s.EgressInterface, "/") {
		c.reject(r, "egressInterface", "required", "an explicit bare resource or kernel interface name is required; dynamic egress selection cannot be frozen")
		return resourceprogram.Resource{}
	}
	ifname := c.aliases[s.EgressInterface]
	if ifname == "" {
		ifname = s.EgressInterface
	}
	for _, link := range c.resources {
		if link.Metadata.Name != s.EgressInterface {
			continue
		}
		switch link.Kind {
		case "DSLiteTunnel":
			ifname = c.filterInterface(r, "egressInterface", s.EgressInterface)
		case "Bridge", "VRF":
			if ifname != link.Metadata.Name {
				c.reject(r, "egressInterface", "unsupported_semantics", "legacy NAT controller uses this virtual link's resource name rather than ifname; make the names agree before migration")
			}
		case "Link":
			if ifname != s.EgressInterface {
				c.reject(r, "egressInterface", "unsupported_semantics", "legacy NAT controller does not resolve Link aliases; use the explicit kernel interface name")
			}
		}
	}
	if len(s.SourceRanges) == 0 {
		c.reject(r, "sourceRanges", "required", "at least one explicit IPv4 source range is required")
	}
	sources := c.packetCIDRs(r, "sourceRanges", s.SourceRanges, 4, false)
	destinations := c.packetCIDRs(r, "destinationCIDRs", s.DestinationCIDRs, 4, false)
	if len(s.DestinationCIDRs) == 0 {
		destinations = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	}
	exclusions := c.packetCIDRs(r, "excludeDestinationCIDRs", s.ExcludeDestinationCIDRs, 4, false)
	if len(sources)*len(destinations) > maxFirewallProjection {
		c.reject(r, "sourceRanges", "resource_limit", "NAT expansion exceeds 8192 native packet rules")
		return resourceprogram.Resource{}
	}
	address := s.SNATAddress
	if s.SNATAddressFrom.Resource != "" {
		if s.SNATAddressFrom.Field == "" {
			c.reject(r, "snatAddressFrom.field", "required", "the original SNAT address source requires its explicit address field")
		}
		if address != "" {
			c.reject(r, "snatAddressFrom", "conflict", "literal and resource-derived SNAT addresses cannot be combined")
		}
		var sourceWhen api.ResourceWhenSpec
		address, sourceWhen = c.resolveAddress(r, "snatAddressFrom", s.SNATAddressFrom, true)
		out.When = egressAll(out.When, c.when(r, sourceWhen, elections))
	} else if meaningful(child(r.node, "snatAddressFrom")) {
		c.reject(r, "snatAddressFrom", "required", "static address source requires a resource reference")
	}
	if s.Type == "masquerade" && address != "" {
		c.reject(r, "snatAddress", "conflict", "masquerade cannot carry an unused SNAT address")
	}
	if s.Type == "snat" {
		a, err := netip.ParseAddr(address)
		if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
			c.reject(r, "snatAddress", "invalid_value", "SNAT requires a literal or resolved static IPv4 unicast address")
		}
	}
	for _, source := range sources {
		for _, destination := range destinations {
			out.NAT = append(out.NAT, network.NATRule{Type: s.Type, Match: network.PacketMatch{Family: 4, Source: source.String(), Destination: destination.String(), ExcludeDestinations: exclusions, OutInterface: ifname}, Address: address})
		}
	}
	c.target(r, "nat/"+r.Metadata.Name)
	c.note(r, "Preserves explicit source NAT without adding filter hooks to NAT-only configurations; destination exclusions remain negated matches within each rule.")
	return out
}
