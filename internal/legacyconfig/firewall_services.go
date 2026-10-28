// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
)

// firewallServices preserves the effective Linux service exceptions from
// pkg/render/firewall_holes.go and writeFirewallPairChain at SourceRevision.
// It only augments an existing INPUT projection; standalone services do not
// acquire a new filtering hook or policy during migration.
func (c *compiler) firewallServices() {
	f := c.document.Network.Firewall
	if f == nil || f.Input == nil {
		return
	}
	// byListenAddress only produces DNS candidates for non-untrust zones.
	// The Linux pair renderer then suppresses all to-self holes for trust/mgmt
	// because their implicit action is accept. Thus none of those candidates
	// bypass an explicit ACL. Resolved addressFrom values must not create new
	// holes either: the old hole index only inspected literal addresses.
	for _, r := range c.resourcesOfKind("DNSResolver") {
		c.note(r, "Preserves existing INPUT ACL decisions: legacy DNS listener candidates exclude untrust, and trust/mgmt self chains suppress automatic holes; resolving addressFrom adds no new firewall exception.")
	}
	// The old index selects zones by the resource reference written in the
	// zone, not by an equivalent kernel-interface alias. Preserve source order
	// here; firewall() separately rejects overlapping zone membership.
	zones := map[string]resource{}
	for _, r := range c.resources {
		if r.Kind != "FirewallZone" || r.APIVersion != api.FirewallAPIVersion {
			continue
		}
		for _, ref := range r.Spec.(api.FirewallZoneSpec).Interfaces {
			// Match splitResourceRef: only the entire reference is trimmed;
			// whitespace inside Kind/name is not a different resource alias.
			ref = strings.TrimSpace(ref)
			if _, name, qualified := strings.Cut(ref, "/"); qualified {
				ref = name
			}
			zones[ref] = r
		}
	}
	pending := map[string][]network.PacketMatch{}
	for _, r := range c.resources {
		var iface string
		var listen []string
		var port int
		match := network.PacketMatch{Protocol: "udp"}
		switch s := r.Spec.(type) {
		case api.DHCPv4ServerSpec:
			iface, listen, port = s.Interface, s.ListenInterfaces, 67
		case api.DHCPv6ServerSpec:
			iface, listen, port = s.Interface, s.ListenInterfaces, 547
		case api.VirtualAddressSpec:
			if s.Mode != "vrrp" {
				continue
			}
			iface, match.Protocol, match.Family = s.Interface, "vrrp", 4
			if s.Family == "ipv6" {
				match.Family = 6
			}
		default:
			continue
		}
		if port != 0 {
			match.DestinationPorts = []network.PortRange{{Start: port}}
		}
		seen := map[string]bool{}
		for _, ref := range append([]string{iface}, listen...) {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			zone, exists := zones[legacyRefName(ref)]
			if !exists || zone.Spec.(api.FirewallZoneSpec).Role != "untrust" {
				continue
			}
			for _, ifname := range c.firewallServiceInterfaces(r, zone, ref) {
				present := false
				for _, old := range pending[ifname] {
					present = present || old.Protocol == match.Protocol && old.Family == match.Family && (port == 0 || old.DestinationPorts[0].Start == port)
				}
				if !present {
					match.InInterface = ifname
					pending[ifname] = append(pending[ifname], match)
				}
			}
		}
		if match.Protocol == "vrrp" {
			c.note(r, "Preserves the original family-scoped protocol112 INPUT exception on the election interface before untrust ACLs; trust/mgmt ACLs remain authoritative.")
		} else {
			c.note(r, "Preserves the original dual-family UDP/"+strconv.Itoa(port)+" INPUT exception before untrust ACLs when the hole index resolves an interface; trust/mgmt ACLs remain authoritative. Empty-interface legacy holes do not create new accepts.")
		}
	}
	if len(pending) == 0 {
		return
	}
	count := 0
	for _, ports := range pending {
		count += len(ports)
	}
	for _, chain := range []*network.FirewallChain{f.Input, f.Output, f.ForwardChain} {
		if chain != nil {
			for _, rule := range chain.Rules {
				count += max(1, len(rule.Match.SourcePorts)) * max(1, len(rule.Match.DestinationPorts)) * max(1, len(rule.Match.ICMPTypes))
			}
		}
	}
	if count > maxFirewallProjection {
		c.issue(nil, "$.spec.resources", "resource_limit", "service exceptions exceed the bounded native firewall projection")
		return
	}
	var rules []network.FirewallRule
	for i, rule := range f.Input.Rules {
		// Keep the original global loopback and ICMPv6 rules first. Each
		// disjoint zone interface gets its exception before its first ACL.
		if matches := pending[rule.Match.InInterface]; i >= 2 && len(matches) != 0 {
			for _, match := range matches {
				rules = append(rules, network.FirewallRule{Match: match, Action: "accept"})
			}
			delete(pending, rule.Match.InInterface)
		}
		rules = append(rules, rule)
	}
	if len(pending) != 0 {
		c.issue(nil, "$.spec.resources", "unsupported_semantics", "service exception has no matching owned INPUT zone projection")
		return
	}
	f.Input.Rules = rules
}

func (c *compiler) firewallServiceInterfaces(server, zone resource, ref string) []string {
	name := legacyRefName(ref)
	scope := ""
	if owner := c.byRef["Interface/"+name]; owner != nil {
		scope = strings.TrimSpace(owner.Spec.(api.InterfaceSpec).IfName)
		if scope == "" {
			c.reject(server, "interface", "unsupported_semantics", "service hole requires the original explicit interface name")
			return nil
		}
	} else if c.byRef["Bridge/"+name] != nil {
		// Bridge is absent from internalFirewallResourceIfName. The old
		// variadic add receives one empty string, and compactStrings preserves
		// it: the renderer emits iifname "", not an unscoped zone-wide rule.
		// That match cannot select a real ingress interface. Omitting the
		// ineffective hole preserves ACL decisions without widening access.
		c.note(server, "The original Bridge service exception rendered an empty iifname match and accepted no real ingress; the migrated INPUT ACL remains authoritative for that bridge and its other zone members.")
		return nil
	} else {
		c.reject(server, "interface", "unsupported_semantics", "service hole requires a migrated Interface or Bridge resource")
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, member := range zone.Spec.(api.FirewallZoneSpec).Interfaces {
		ifname := c.filterInterface(zone, "interfaces", member)
		if ifname != "" && !seen[ifname] && ifname == scope {
			seen[ifname] = true
			out = append(out, ifname)
		}
	}
	if len(out) == 0 {
		c.reject(server, "interface", "unsupported_semantics", "legacy service hole scope differs from the owned zone interface projection")
		return nil
	}
	c.target(server, "network.firewall.input")
	return out
}
