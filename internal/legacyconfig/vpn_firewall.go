// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"sort"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
)

// WireGuard inserts an IPv4 iptables ACCEPT. Both WireGuard and Tailscale
// contribute to the same routerd-owned nftables listener-hole index. Its
// filter independently allows both families on the first interface found by its
// hole index in the name-sorted first untrust zone, before that zone's ACL. If
// that index finds no interface, the original empty iifname matches no ingress.
// An iptables ACCEPT cannot override a drop in another base chain. Preserve that effective
// owned-policy scope instead of opening every untrust interface or bypassing
// explicit trust/mgmt ACLs. With no owned filter, a new native INPUT chain would
// also introduce the mandatory conntrack INVALID drop, unlike the old listener
// rule. Reject that standalone WireGuard case instead of inventing unrelated
// stateful policy. A standalone Tailscale declaration adds no INPUT hook.
// No foreign firewall rules are imported or changed.
func (c *compiler) vpnListenerFirewall() {
	ports := map[int]bool{}
	for _, device := range c.document.Network.WireGuard {
		if device.ListenPort > 0 && device.ListenPort <= 65535 {
			ports[device.ListenPort] = true
		}
	}
	for _, service := range c.document.Services {
		if service.Tailscale != nil {
			ports[41641] = true
		}
	}
	if len(ports) == 0 {
		return
	}
	var ordered []int
	for port := range ports {
		ordered = append(ordered, port)
	}
	sort.Ints(ordered)
	f := c.document.Network.Firewall
	if f == nil || f.Input == nil {
		for _, r := range c.resourcesOfKind("WireGuardInterface") {
			if s := r.Spec.(api.WireGuardInterfaceSpec); s.ListenPort > 0 {
				c.reject(r, "listenPort", "unsupported_semantics", "the standalone legacy IPv4 listener exception cannot be represented without adding a native stateful INPUT drop; declare and review an explicit FirewallZone policy or native firewall intent")
			}
		}
		return
	}
	var firstZone *resource
	for _, r := range c.resourcesOfKind("FirewallZone") {
		if r.Spec.(api.FirewallZoneSpec).Role == "untrust" {
			copy := r
			firstZone = &copy
			break
		}
	}
	if firstZone == nil {
		c.vpnListenerFirewallNotes("Preserves the original owned INPUT policy: without an untrust zone no nftables listener exception existed; trust/mgmt ACLs remain authoritative.")
		return
	}
	interfaces := c.vpnListenerInterfaces(*firstZone)
	if len(interfaces) == 0 {
		return
	}
	count := len(interfaces) * len(ordered)
	for _, chain := range []*network.FirewallChain{f.Input, f.Output, f.ForwardChain} {
		if chain != nil {
			for _, rule := range chain.Rules {
				count += max(1, len(rule.Match.SourcePorts)) * max(1, len(rule.Match.DestinationPorts)) * max(1, len(rule.Match.ICMPTypes))
			}
		}
	}
	if count > maxFirewallProjection {
		c.issue(nil, "$.spec.resources", "resource_limit", "VPN listener exceptions exceed the bounded native firewall projection")
		return
	}
	pending := map[string]bool{}
	for _, name := range interfaces {
		pending[name] = true
	}
	var rules []network.FirewallRule
	for i, rule := range f.Input.Rules {
		if i >= 2 && pending[rule.Match.InInterface] {
			for _, port := range ordered {
				rules = append(rules, network.FirewallRule{Match: network.PacketMatch{InInterface: rule.Match.InInterface, Protocol: "udp", DestinationPorts: []network.PortRange{{Start: port}}}, Action: "accept"})
			}
			delete(pending, rule.Match.InInterface)
		}
		rules = append(rules, rule)
	}
	if len(pending) != 0 {
		c.reject(*firstZone, "interfaces", "unsupported_semantics", "VPN listener exception has no matching owned INPUT zone projection")
		return
	}
	f.Input.Rules = rules
	c.vpnListenerFirewallNotes("Preserves the original dual-family UDP listener exception before ACLs on the first hole-index interface of the name-sorted first untrust zone. Other interfaces, zones and trust/mgmt ACLs are not bypassed.")
}

// The original hole index intentionally differs from the filter alias builder:
// Bridge is a valid filter member but is absent from internalFirewallResourceIfName.
// Do not make a leading bridge steal the listener from a later Interface, or
// widen a scoped hole to every member. For a bridge-only zone, the variadic add
// receives []string{""}; compactStrings retains that entry, so nftables renders
// iifname "". It is an ineffective match, not an unscoped zone-wide exception.
func (c *compiler) vpnListenerInterfaces(zone resource) []string {
	owners := map[string]resource{}
	for _, r := range c.resources {
		owners[r.ID()] = r
	}
	selected := ""
	for _, ref := range zone.Spec.(api.FirewallZoneSpec).Interfaces {
		name := c.filterInterface(zone, "interfaces", ref)
		if name == "" {
			return nil
		}
		if selected != "" {
			continue
		}
		owner := owners[c.aliasOwner[legacyRefName(ref)]]
		candidate := ""
		switch s := owner.Spec.(type) {
		case api.InterfaceSpec:
			candidate = strings.TrimSpace(s.IfName)
		case api.DSLiteTunnelSpec:
			if api.BoolDefault(s.Enabled, true) {
				candidate = strings.TrimSpace(s.TunnelName)
			}
		case api.WireGuardInterfaceSpec:
			candidate = strings.TrimSpace(first(s.IfName, owner.Metadata.Name))
		}
		if candidate != "" {
			if candidate != name {
				c.reject(zone, "interfaces", "unsupported_semantics", "legacy filter and listener-hole interface names differ; use explicit matching kernel interface names before migration")
				return nil
			}
			selected = candidate
		}
	}
	if selected != "" {
		return []string{selected}
	}
	c.vpnListenerFirewallNotes("The first untrust zone has no interface in the original listener-hole index, so its VPN listener exception rendered an empty iifname match and accepted no real ingress. The migrated INPUT ACL remains authoritative; no new accept is added.")
	return nil
}

func (c *compiler) vpnListenerFirewallNotes(note string) {
	for _, r := range c.resourcesOfKind("WireGuardInterface") {
		if s := r.Spec.(api.WireGuardInterfaceSpec); s.ListenPort > 0 {
			c.target(r, "network.firewall.input")
			c.note(r, note)
		}
	}
	for _, r := range c.resourcesOfKind("TailscaleNode") {
		if r.Spec.(api.TailscaleNodeSpec).State != "absent" {
			c.target(r, "network.firewall.input")
			c.note(r, note)
		}
	}
}
