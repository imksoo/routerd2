package network

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
)

func validateRoutes(routes []Route) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seenRoutes := map[string]bool{}
	for _, r := range routes {
		p, err := netip.ParsePrefix(r.Destination)
		if err != nil || !p.IsValid() || p.Addr().Is4In6() {
			add("route: invalid destination %q", r.Destination)
			continue
		}
		if p != p.Masked() {
			add("route %s: destination must be a network prefix", r.Destination)
		}
		if r.Metric < 0 || uint64(r.Metric) > uint64(^uint32(0)) {
			add("route %s: invalid metric", r.Destination)
		}
		if r.Table < 0 || uint64(r.Table) > uint64(^uint32(0)) || r.Table == 255 {
			add("route %s: invalid/reserved route table", r.Destination)
		}
		validateHop := func(gateway, name string) {
			if !validName(name) {
				add("route %s: invalid next-hop interface %q", r.Destination, name)
			}
			if gateway != "" {
				g, err := netip.ParseAddr(gateway)
				if err != nil || !g.IsValid() || g.Is4In6() || g.Zone() != "" || g.IsUnspecified() || g.IsMulticast() || g.Is4() != p.Addr().Is4() {
					add("route %s: gateway must be a same-family unicast address", r.Destination)
				}
			}
		}
		if len(r.NextHops) == 0 {
			validateHop(r.Gateway, r.Interface)
		} else {
			if r.Gateway != "" || r.Interface != "" {
				add("route %s: next_hops is mutually exclusive with gateway/interface", r.Destination)
			}
			seen := map[string]bool{}
			for _, h := range r.NextHops {
				validateHop(h.Gateway, h.Interface)
				if h.Weight < 1 || h.Weight > 256 {
					add("route %s: next-hop weight must be 1..256", r.Destination)
				}
				g := h.Gateway
				if a, err := netip.ParseAddr(g); err == nil {
					g = a.String()
				}
				key := h.Interface + "/" + g
				if seen[key] {
					add("route %s: duplicate next hop %s on %s", r.Destination, g, h.Interface)
				}
				seen[key] = true
			}
		}
		key := routeSlot(r)
		if seenRoutes[key] {
			add("route %s: duplicate destination/table/metric; use next_hops for ECMP", r.Destination)
		}
		seenRoutes[key] = true
	}
	return errors.Join(errs...)
}

func normalizeRoute(r Route) Route {
	if p, err := netip.ParsePrefix(r.Destination); err == nil {
		r.Destination = p.Masked().String()
		if p.Addr().Is6() && r.Metric == 0 {
			r.Metric = 1024
		}
	}
	r.Table = routeTable(r)
	if g, err := netip.ParseAddr(r.Gateway); err == nil {
		r.Gateway = g.String()
	}
	r.NextHops = slices.Clone(r.NextHops)
	for i := range r.NextHops {
		if g, err := netip.ParseAddr(r.NextHops[i].Gateway); err == nil {
			r.NextHops[i].Gateway = g.String()
		}
	}
	// Kernel dump order is not ownership. Sort a copy so comparison never
	// mutates the caller's candidate or the durable journal.
	sort.Slice(r.NextHops, func(i, j int) bool {
		a, b := r.NextHops[i], r.NextHops[j]
		if a.Interface != b.Interface {
			return a.Interface < b.Interface
		}
		if a.Gateway != b.Gateway {
			return a.Gateway < b.Gateway
		}
		return a.Weight < b.Weight
	})
	if len(r.NextHops) == 1 && r.Gateway == "" && r.Interface == "" {
		r.Gateway, r.Interface = r.NextHops[0].Gateway, r.NextHops[0].Interface
		r.NextHops = nil
	}
	return r
}

func sameRoute(a, b Route) bool {
	a, b = normalizeRoute(a), normalizeRoute(b)
	return a.Destination == b.Destination && a.Gateway == b.Gateway && a.Interface == b.Interface && a.Metric == b.Metric && a.Table == b.Table && slices.Equal(a.NextHops, b.NextHops)
}

func routeUsesInterface(r Route, name string) bool {
	if r.Interface == name {
		return true
	}
	for _, h := range r.NextHops {
		if h.Interface == name {
			return true
		}
	}
	return false
}
