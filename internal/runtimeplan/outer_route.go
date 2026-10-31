package runtimeplan

import (
	"net/netip"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

// outerIPv4Route pins a tunnel endpoint to the underlay path that already
// reaches it. A plain device route is correct only for an on-link endpoint;
// remote peers retain the acquired gateway or a configured underlay route.
// An empty route means a pre-existing /32 already supplies that pin.
func outerIPv4Route(desired network.Desired, remoteText, underlay string, lease *leasedIPv4, clock boottime.Clock, until lifetime.Deadline) (network.Route, bool) {
	remote, err := netip.ParseAddr(remoteText)
	if err != nil || !remote.Is4() || remote.IsUnspecified() || remote.IsMulticast() {
		return network.Route{}, false
	}
	destination := netip.PrefixFrom(remote, 32).String()
	domain := network.RoutingDomains(desired)[underlay]
	if endpointOnUnderlay(desired, remote, underlay) {
		return finiteOuterRoute(network.Route{Destination: destination, Interface: underlay, Table: domain.Table}, until), true
	}
	if route, exact, ok := configuredUnderlayRoute(desired, remote, underlay, clock); ok {
		if exact {
			return network.Route{}, true
		}
		route.Destination = destination
		return finiteOuterRoute(route, until), true
	}
	if lease != nil && lease.gateway.IsValid() {
		return finiteOuterRoute(network.Route{Destination: destination, Gateway: lease.gateway.String(), Interface: underlay, Table: domain.Table}, until), true
	}
	return network.Route{}, false
}

func finiteOuterRoute(route network.Route, until lifetime.Deadline) network.Route {
	route.BootID, route.ExpiresNanos = until.BootID, until.Nanos
	route.ValidUntil = lifetime.Deadline{}
	return route
}

func endpointOnUnderlay(desired network.Desired, remote netip.Addr, underlay string) bool {
	for _, iface := range desired.Interfaces {
		if iface.Name != underlay {
			continue
		}
		for _, raw := range iface.Addresses {
			prefix, err := netip.ParsePrefix(raw)
			if err == nil && prefix.Addr().Is4() && prefix.Contains(remote) {
				return true
			}
		}
	}
	return false
}

func configuredUnderlayRoute(desired network.Desired, remote netip.Addr, underlay string, clock boottime.Clock) (network.Route, bool, bool) {
	var selected network.Route
	selectedBits, selectedMetric := -1, 0
	for _, route := range desired.Routes {
		if route.Blackhole || route.BootID != "" && (route.BootID != clock.BootID || route.ExpiresNanos <= uint64(clock.BoottimeNanoseconds)) {
			continue
		}
		prefix, err := netip.ParsePrefix(route.Destination)
		if err != nil || !prefix.Addr().Is4() || !prefix.Contains(remote) {
			continue
		}
		candidate, matches := routeOnUnderlay(route, underlay)
		if !matches || selectedBits > prefix.Bits() || selectedBits == prefix.Bits() && selectedMetric <= candidate.Metric {
			continue
		}
		selected, selectedBits, selectedMetric = candidate, prefix.Bits(), candidate.Metric
	}
	if selectedBits < 0 {
		return network.Route{}, false, false
	}
	return selected, selectedBits == 32, true
}

func routeOnUnderlay(route network.Route, underlay string) (network.Route, bool) {
	if route.Interface == underlay {
		route.NextHops = nil
		return route, true
	}
	var hops []network.NextHop
	for _, hop := range route.NextHops {
		if hop.Interface == underlay {
			hops = append(hops, hop)
		}
	}
	if len(hops) == 0 {
		return network.Route{}, false
	}
	if len(hops) == 1 {
		route.Gateway, route.Interface, route.NextHops = hops[0].Gateway, hops[0].Interface, nil
		return route, true
	}
	route.Gateway, route.Interface, route.NextHops = "", "", hops
	return route, true
}
