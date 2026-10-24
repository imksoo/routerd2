package runtimeplan

import (
	"fmt"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// The native route API has a finite kernel expiry range. Long protocol
// lifetimes retain their original value in the accepted snapshot; only their
// kernel grant uses a renewable horizon. Daily renewal leaves at least six
// days of kernel lifetime while avoiding per-tick route replacement.
func raKernelDeadline(deadline, now time.Time) time.Time {
	if deadline.IsZero() {
		return time.Time{} // explicitly advertised infinity
	}
	horizon := now.UTC().Truncate(24 * time.Hour).Add(7 * 24 * time.Hour)
	if deadline.After(horizon) {
		return horizon
	}
	return deadline
}

func raLive(deadline, now time.Time) bool { return deadline.IsZero() || now.Before(deadline) }

func raEarlier(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

func projectRAClient(p *projection, service serviceconfig.Service, snapshot raclient.Snapshot, has bool, now time.Time) error {
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
	if !has || !snapshot.Active {
		return nil
	}
	for _, address := range snapshot.Addresses {
		if address.DADFailed || !raLive(address.ValidUntil, now) {
			continue
		}
		if err := addAddressWithOptions(&p.network, c.Interface, address.Address, network.AddressOptions{PreferredUntil: address.PreferredUntil, ValidUntil: address.ValidUntil}); err != nil {
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
		if !raLive(prefix.ValidUntil, now) {
			continue
		}
		onLink[prefix.Prefix] = true
		p.network.Routes = append(p.network.Routes, network.Route{Destination: prefix.Prefix, Interface: c.Interface, Table: table, Metric: metric, ValidUntil: raKernelDeadline(prefix.ValidUntil, now)})
	}
	groups := map[string][]raclient.Route{}
	for _, route := range snapshot.Routes {
		if !raLive(route.ValidUntil, now) || onLink[route.Destination] {
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
		var deadline time.Time
		for _, member := range group {
			deadline = raEarlier(deadline, member.ValidUntil)
		}
		out := network.Route{Destination: destination, Table: table, Metric: metric, ValidUntil: raKernelDeadline(deadline, now)}
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
