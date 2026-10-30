package runtimeplan

import (
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func projectIPIPs(plans []resourceprogram.IPIPPlan, addresses map[string]leasedIPv4, clock boottime.Clock, p *projection) {
	for _, plan := range plans {
		state := p.resources[plan.ID]
		if !state.Active {
			continue
		}
		source, ok := addresses[plan.Source]
		if !ok || !source.validUntil.Live(clock) {
			continue
		}
		until := lifetime.Minimum(clock, source.validUntil, state.ActiveUntil)
		link := plan.Link
		link.Local, link.Authority = source.address.String(), until
		p.network.Links = append(p.network.Links, link)
		// The leased endpoint already entered this projection as a typed address.
		// The link and inner address share the resource's original lease deadline.
		options := network.AddressOptions{BootLifetime: network.BootLifetime{BootID: until.BootID, PreferredNanos: until.Nanos, ValidNanos: until.Nanos}}
		ensureInterface(&p.network, link.Name)
		for i := range p.network.Interfaces {
			if p.network.Interfaces[i].Name == link.Name {
				iface := &p.network.Interfaces[i]
				iface.MTU = plan.MTU
				address := plan.Address.String()
				iface.Addresses = append(iface.Addresses, address)
				if iface.AddressOptions == nil {
					iface.AddressOptions = make(map[string]network.AddressOptions)
				}
				iface.AddressOptions[address] = options
				break
			}
		}
	}
}
