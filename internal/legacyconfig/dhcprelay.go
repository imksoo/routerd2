package legacyconfig

import (
	"net/netip"
	"slices"
	"sort"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) dhcpRelays() {
	var relays []resource
	for _, r := range c.resources {
		if r.Kind == "DHCPv4Relay" {
			relays = append(relays, r)
		}
	}
	if len(relays) == 0 {
		return
	}
	sort.Slice(relays, func(i, j int) bool { return relays[i].Metadata.Name < relays[j].Metadata.Name })
	service := serviceconfig.Service{Name: c.serviceName(relays[0], "relay"), DHCPRelay: &dhcprelayconfig.Config{}}
	byName := map[string]int{}
	for _, r := range relays {
		c.allow(r, "interfaces", "upstream")
		spec := r.Spec.(api.DHCPv4RelaySpec)
		if len(spec.Interfaces) == 0 {
			c.reject(r, "interfaces", "required", "DHCP relay needs explicit client interfaces")
		}
		for _, ref := range spec.Interfaces {
			name := c.dhcpInterface(r, "interfaces", ref)
			iface := c.ensureInterface(name)
			if iface != nil && iface.Master != nil && *iface.Master != "" {
				c.reject(r, "interfaces", "unsupported_semantics", "legacy relay VRF binding is ambiguous; declare the native routing domain explicitly")
			}
			var source netip.Addr
			count := 0
			for _, static := range c.static {
				if static.iface == name && !meaningful(child(static.resource.node, "when")) {
					source = static.prefix.Addr()
					count++
				}
			}
			if count != 1 {
				c.reject(r, "interfaces", "ambiguous_reference", "relay giaddr requires one unconditional owned IPv4StaticAddress on each client interface")
			}
			index, exists := byName[name]
			if !exists {
				index = len(service.DHCPRelay.Interfaces)
				byName[name] = index
				service.DHCPRelay.Interfaces = append(service.DHCPRelay.Interfaces, dhcprelayconfig.Interface{Name: name, Address: source.String()})
			}
			servers := &service.DHCPRelay.Interfaces[index].Servers
			if !slices.Contains(*servers, spec.Upstream) {
				*servers = append(*servers, spec.Upstream)
			}
		}
		c.target(r, "services/"+service.Name)
		c.note(r, "Client interfaces share one native relay listener; upstream selection remains specific to each interface. No lease state is imported.")
	}
	c.addService(relays[0], service)
}
