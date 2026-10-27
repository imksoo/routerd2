package runtimeplan

import (
	"fmt"
	"sort"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func projectRAClient(p *projection, service serviceconfig.Service, snapshot raclient.Snapshot, has bool, clock boottime.Clock) error {
	c := service.RAClient
	ensureInterface(&p.network, c.Interface)
	for i := range p.network.Interfaces {
		link := &p.network.Interfaces[i]
		if link.Name != c.Interface {
			continue
		}
		if link.IPv6AcceptRA != nil && *link.IPv6AcceptRA != 0 || link.IPv6Autoconf != nil && *link.IPv6Autoconf {
			return fmt.Errorf("RA client %s conflicts with kernel RA/autoconf on %s", service.Name, c.Interface)
		}
		zero, disabled := 0, false
		link.IPv6AcceptRA, link.IPv6Autoconf = &zero, &disabled
	}
	if !has || !snapshot.Active || snapshot.BootID != clock.BootID || snapshot.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return nil
	}
	for i := range p.network.Interfaces {
		iface := &p.network.Interfaces[i]
		if iface.Name != c.Interface {
			continue
		}
		if c.UseMTU && snapshot.MTU != 0 {
			mtu := snapshot.MTU
			if iface.IPv6MTU != nil {
				mtu = min(mtu, *iface.IPv6MTU)
			}
			iface.IPv6MTU = &mtu
		}
		if c.UseHopLimit && snapshot.CurrentHopLimit != 0 {
			hop := snapshot.CurrentHopLimit
			iface.IPv6HopLimit = &hop
		}
	}
	for _, address := range snapshot.Addresses {
		if address.DADFailed || !address.ValidUntil.Live(clock) {
			continue
		}
		if err := addAddressWithOptions(&p.network, c.Interface, address.Address, addressLifetimes(address.PreferredUntil, address.ValidUntil)); err != nil {
			return err
		}
	}
	table, metric := c.Table, c.Metric
	if table == 0 {
		table = 254
		// Derive only a VRF explicitly present in the complete desired plan.
		// Existing foreign kernel state is not silently adopted as intent.
		for _, iface := range p.network.Interfaces {
			if iface.Name == c.Interface && iface.Master != nil {
				for _, link := range p.network.Links {
					if link.Name == *iface.Master && link.Kind == "vrf" {
						table = link.VRFTable
					}
				}
			}
		}
	}
	if metric == 0 {
		metric = 200
	}
	onLink := map[string]bool{}
	for _, prefix := range snapshot.Prefixes {
		if !prefix.ValidUntil.Live(clock) {
			continue
		}
		onLink[prefix.Prefix] = true
		p.network.Routes = append(p.network.Routes, network.Route{Destination: prefix.Prefix, Interface: c.Interface, Table: table, Metric: metric, ValidUntil: prefix.ValidUntil})
	}
	groups := map[string][]raclient.Route{}
	for _, route := range snapshot.Routes {
		if !route.ValidUntil.Live(clock) || onLink[route.Destination] {
			continue
		}
		group := groups[route.Destination]
		if len(group) == 0 || route.Preference > group[0].Preference {
			groups[route.Destination] = []raclient.Route{route}
		} else if route.Preference == group[0].Preference {
			groups[route.Destination] = append(group, route)
		}
	}
	destinations := make([]string, 0, len(groups))
	for destination := range groups {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	for _, destination := range destinations {
		group := groups[destination]
		sort.Slice(group, func(i, j int) bool { return group[i].Gateway < group[j].Gateway })
		var deadline lifetime.Deadline
		for _, member := range group {
			deadline = lifetime.Minimum(clock, deadline, member.ValidUntil)
		}
		out := network.Route{Destination: destination, Table: table, Metric: metric, ValidUntil: deadline}
		if len(group) == 1 {
			out.Gateway, out.Interface = group[0].Gateway, c.Interface
		} else {
			for _, member := range group {
				out.NextHops = append(out.NextHops, network.NextHop{Gateway: member.Gateway, Interface: c.Interface, Weight: 1})
			}
		}
		p.network.Routes = append(p.network.Routes, out)
	}
	return nil
}
