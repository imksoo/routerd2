package resourceprogram

import (
	"net/netip"
	"slices"
)

// inferredDependencies connects typed references that have declarations in this
// program. External physical interfaces remain valid references: they are not
// adopted merely because a route or service names them. Protocol-state edges
// and address readiness are not inferred from a coincidental name or prefix.
func inferredDependencies(resources []Resource) map[string][]string {
	interfaces, links := map[string]string{}, map[string]string{}
	addresses := map[string][]string{}
	for _, r := range resources {
		switch {
		case r.Interface != nil:
			interfaces[r.Interface.Name] = r.ID
		case r.Link != nil:
			links[r.Link.Name] = r.ID
		case r.DSLite != nil:
			links[r.DSLite.Name] = r.ID
		case r.Address != nil:
			if p, err := netip.ParsePrefix(r.Address.CIDR); err == nil {
				key := r.Address.Interface + "\x00" + p.Addr().String()
				addresses[key] = append(addresses[key], r.ID)
			}
		}
	}
	graph := make(map[string][]string, len(resources))
	for _, r := range resources {
		deps := map[string]bool{}
		add := func(id string) {
			if id != "" && id != r.ID {
				deps[id] = true
			}
		}
		iface := func(name string) {
			if id := interfaces[name]; id != "" {
				add(id)
			} else {
				add(links[name])
			}
		}
		switch {
		case r.Interface != nil:
			add(links[r.Interface.Name])
			if r.Interface.Master != nil {
				add(links[*r.Interface.Master])
			}
		case r.Link != nil:
			iface(r.Link.Parent)
		case r.Address != nil:
			iface(r.Address.Interface)
		case r.Route != nil:
			iface(r.Route.Interface)
			for _, hop := range r.Route.NextHops {
				iface(hop.Interface)
			}
		case r.PolicyRule != nil:
			iface(r.PolicyRule.InInterface)
			iface(r.PolicyRule.OutInterface)
		case r.DSLite != nil:
			iface(r.DSLite.Underlay)
		case r.WireGuard != nil:
			add(links[r.WireGuard.Interface])
		case r.VRRPGuard != nil:
			iface(r.VRRPGuard.Interface)
			for _, raw := range r.VRRPGuard.Addresses {
				ip, err := netip.ParseAddr(raw)
				if err != nil {
					continue
				}
				for _, id := range addresses[r.VRRPGuard.Interface+"\x00"+ip.String()] {
					add(id)
				}
			}
		case r.Service != nil:
			iface(r.Service.Interface())
			if r.Service.Health != nil && r.Service.Health.SourceAddress != "" {
				if source, err := netip.ParseAddr(r.Service.Health.SourceAddress); err == nil {
					for _, id := range addresses[r.Service.Health.Interface+"\x00"+source.String()] {
						add(id)
					}
				}
			}
			if r.Service.DHCP6 != nil {
				for _, delegation := range r.Service.DHCP6.Delegations {
					iface(delegation.Interface)
				}
			}
		}
		for dep := range deps {
			graph[r.ID] = append(graph[r.ID], dep)
		}
		slices.Sort(graph[r.ID])
	}
	return graph
}
