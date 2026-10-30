// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/conntrackconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// The old SystemdUnitController runs conntrackd on both peers, independently of
// NAT rule activation. The native IPv4 election owns that same engine lifetime
// and commits the replicated kernel state before publishing its elected links.
func (c *compiler) conntrackSync(nodes []resourceprogram.Resource) {
	resources := c.resourcesOfKind("NAT44SessionSync")
	for _, r := range resources {
		c.allow(r, "mode", "natRules", "conntrackd")
		s := r.Spec.(api.NAT44SessionSyncSpec)
		if len(resources) != 1 {
			c.reject(r, "", "unsupported_semantics", "one conntrack sync engine must own the shared kernel connection table; multiple declarations cannot be assigned to independent elections")
			continue
		}
		if s.Mode != "conntrackd" || s.Conntrackd == nil {
			c.reject(r, "mode", "unsupported_semantics", "only explicit conntrackd replication has a native migration")
			continue
		}
		if len(s.NATRules) == 0 {
			c.reject(r, "natRules", "required", "NAT rule references must identify the single IPv4 VRRP owner")
			continue
		}
		owner := -1
		valid := true
		for _, ref := range s.NATRules {
			rule := c.getRef(r, "natRules", ref, "NAT44Rule")
			if rule == nil {
				valid = false
				continue
			}
			var when *resourceprogram.FactCondition
			for _, node := range nodes {
				if node.ID == "nat/"+rule.Metadata.Name {
					when = node.When
					break
				}
			}
			selected := -1
			for i, service := range c.document.Services {
				if service.VRRP == nil || service.VRRP.Family != "ipv4" || !natRequiresElection(when, service.ModuleName()+".elected") {
					continue
				}
				if selected >= 0 {
					selected = -1
					break
				}
				selected = i
			}
			if selected < 0 || owner >= 0 && owner != selected {
				c.reject(r, "natRules", "unsupported_semantics", "every referenced NAT rule must require the same single IPv4 VRRP election; an absent or ambiguous owner cannot be inferred")
				valid = false
				continue
			}
			owner = selected
		}
		if !valid {
			continue
		}
		source := s.Conntrackd
		cfg := conntrackconfig.Config{Interface: c.interfaceName(r, "conntrackd.interface", source.Interface, true)}
		var err error
		cfg.LocalAddress, err = netip.ParseAddr(strings.TrimSpace(source.LocalAddress))
		if err != nil {
			c.reject(r, "conntrackd.localAddress", "invalid_value", "conntrack local address must be an IPv4 address")
		}
		cfg.PeerAddress, err = netip.ParseAddr(strings.TrimSpace(source.PeerAddress))
		if err != nil {
			c.reject(r, "conntrackd.peerAddress", "invalid_value", "conntrack peer address must be an IPv4 address")
		}
		if source.Port < 0 || source.Port > 65535 {
			c.reject(r, "conntrackd.port", "invalid_value", "conntrack port must be 1..65535 or omitted for the default")
		} else {
			cfg.Port = uint16(source.Port)
		}
		for _, value := range source.IgnoreIPv4 {
			address, err := netip.ParseAddr(strings.TrimSpace(value))
			if err != nil {
				c.reject(r, "conntrackd.ignoreIPv4", "invalid_value", "conntrack exclusions must be IPv4 addresses")
				continue
			}
			cfg.IgnoreIPv4 = append(cfg.IgnoreIPv4, address)
		}
		c.document.Services[owner].VRRP.ConntrackSync = &cfg
		c.target(r, "services/"+c.document.Services[owner].Name)
		c.note(r, "The referenced NAT rules select one IPv4 VRRP owner. Its existing conntrackd engine runs on both peers and commits replicated connection state before activation. The conntrackd interface, peer, port and IPv4 exclusions retain the original renderer semantics; natRules identifies ownership, not a per-rule replication filter. No SSH/event-stream sender or second sync engine is created.")
	}
}

// A positive election must hold on every path that enables this NAT rule.
// Other readiness conditions may narrow activation without changing ownership.
func natRequiresElection(when *resourceprogram.FactCondition, election string) bool {
	if when == nil {
		return false
	}
	if when.Fact == election {
		return true
	}
	for i := range when.All {
		if natRequiresElection(&when.All[i], election) {
			return true
		}
	}
	if len(when.Any) == 0 {
		return false
	}
	for i := range when.Any {
		if !natRequiresElection(&when.Any[i], election) {
			return false
		}
	}
	return true
}
