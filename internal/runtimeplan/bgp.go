package runtimeplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type RouteIssue struct {
	ReasonTruncated bool   `json:"reason_truncated,omitempty"`
	Service         string `json:"service"`
	Prefix          string `json:"prefix"`
	Reason          string `json:"reason"`
}
type routeGroup struct {
	service       string
	prefix        netip.Prefix
	table, metric int
	paths         []bgpconfig.Route
}
type fibCandidate struct {
	prefix        netip.Prefix
	table, metric int
	hops          []network.NextHop
	connected     bool
}
type fibIndex map[int]map[netip.Prefix][]fibCandidate

func (i fibIndex) add(c fibCandidate) {
	if i[c.table] == nil {
		i[c.table] = map[netip.Prefix][]fibCandidate{}
	}
	i[c.table][c.prefix] = append(i[c.table][c.prefix], c)
}
func routeTable(table int) int {
	if table == 0 {
		return 254
	}
	return table
}
func routeMetric(metric int, ip netip.Addr) int {
	if metric == 0 && ip.Is6() {
		return 1024
	}
	return metric
}

// projectBGP installs only resolvable selected paths. An unresolved next hop is
// visible as a pending RouteIssue; it never installs a speculative onlink route
// or kills a healthy BGP session. Kernel apply errors still fail the ACK.
// Resolution uses this complete committed projection, excluding foreign routes.
func projectBGP(p *projection, doc config.Native, records map[string]serviceconfig.Update, now time.Time, clock bfd.Clock) error {
	groups := map[string]*routeGroup{}
	sources := collectBFD(doc.Services, records)
	for _, service := range doc.Services {
		if service.BGP == nil {
			continue
		}
		u, ok := records[service.ModuleName()]
		if !ok {
			continue
		}
		var s bgpconfig.Snapshot
		if err := json.Unmarshal(u.Snapshot, &s); err != nil {
			return err
		}
		if !s.Active || !now.Before(s.ValidUntil) {
			continue
		}
		c := service.BGPConfig("/run/routerd", "/var/lib/routerd")
		if len(s.Routes) > c.MaxRoutes {
			return errors.New("BGP selected routes exceed configured limit")
		}
		for _, route := range s.Routes {
			if ref, required := c.PeerBFD[route.PeerAddress]; required {
				if ref.Address == "" {
					ref.Address = route.PeerAddress
				}
				if !acknowledgedBFD(sources[ref.Service].grant(ref, clock), s.BFD[route.PeerAddress]) {
					// Independently withdraw the FIB path while the BGP worker
					// closes its session. The route's own ownership heartbeat
					// cannot extend a BFD receive deadline.
					continue
				}
			}
			prefix, err := netip.ParsePrefix(route.Prefix)
			if err != nil || prefix != prefix.Masked() {
				return fmt.Errorf("BGP %s has an invalid selected prefix", service.Name)
			}
			hop, err := netip.ParseAddr(route.NextHop)
			if err != nil || hop.Is4In6() || hop.Zone() != "" || hop.IsUnspecified() || hop.IsMulticast() || prefix.Addr().Is4() != hop.Is4() {
				return fmt.Errorf("BGP %s has an invalid next hop", service.Name)
			}
			key := fmt.Sprintf("%s/%d/%d", prefix, c.Table, c.RouteMetric)
			group := groups[key]
			if group == nil {
				group = &routeGroup{service: service.Name, prefix: prefix, table: c.Table, metric: c.RouteMetric}
				groups[key] = group
			} else if group.service != service.Name {
				return fmt.Errorf("BGP instances %s and %s compete for prefix/table/metric %s", group.service, service.Name, key)
			}
			group.paths = append(group.paths, route)
		}
	}
	index := fibIndex{}
	vrfs := map[string]int{}
	for _, l := range p.network.Links {
		if l.Kind == "vrf" {
			vrfs[l.Name] = l.VRFTable
		}
	}
	for _, l := range p.network.Interfaces {
		table := 254
		if l.Master != nil && vrfs[*l.Master] != 0 {
			table = vrfs[*l.Master]
		}
		for _, value := range l.Addresses {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return err
			}
			index.add(fibCandidate{prefix: prefix.Masked(), table: table, connected: true, hops: []network.NextHop{{Interface: l.Name, Weight: 1}}})
		}
	}
	for _, r := range p.network.Routes {
		prefix, err := netip.ParsePrefix(r.Destination)
		if err != nil {
			return err
		}
		hops := r.NextHops
		if len(hops) == 0 {
			hops = []network.NextHop{{Gateway: r.Gateway, Interface: r.Interface, Weight: 1}}
		}
		index.add(fibCandidate{prefix: prefix, table: routeTable(r.Table), metric: routeMetric(r.Metric, prefix.Addr()), hops: hops})
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := groups[key]
		candidate := fibCandidate{prefix: g.prefix, table: g.table, metric: g.metric}
		for _, path := range g.paths {
			candidate.hops = append(candidate.hops, network.NextHop{Gateway: path.NextHop, Interface: path.Interface, Weight: 1})
		}
		index.add(candidate)
	}
	for _, key := range keys {
		g := groups[key]
		var next []network.NextHop
		var reasons []string
		for _, path := range g.paths {
			hops, err := index.resolve(netip.MustParseAddr(path.NextHop), g.table, path.Interface, map[string]bool{}, 0)
			if err != nil {
				reasons = append(reasons, err.Error())
				continue
			}
			next = append(next, hops...)
		}
		if len(reasons) > 0 {
			p.routeIssues = append(p.routeIssues, RouteIssue{Service: g.service, Prefix: g.prefix.String(), Reason: strings.Join(unique(reasons), "; ")})
		}
		next = uniqueHops(next)
		if len(next) == 0 {
			continue
		}
		route := network.Route{Destination: g.prefix.String(), Table: g.table, Metric: g.metric}
		if len(next) == 1 {
			route.Gateway = next[0].Gateway
			route.Interface = next[0].Interface
		} else {
			route.NextHops = next
		}
		p.network.Routes = append(p.network.Routes, route)
	}
	return nil
}
func (i fibIndex) resolve(target netip.Addr, table int, hint string, seen map[string]bool, depth int) ([]network.NextHop, error) {
	key := fmt.Sprintf("%d:%s%%%s", table, target, hint)
	if seen[key] || depth > 16 {
		return nil, fmt.Errorf("next-hop resolution loop for %s in table %d", target, table)
	}
	if target.IsLinkLocalUnicast() {
		if hint == "" {
			return nil, fmt.Errorf("link-local next hop %s needs an explicit interface", target)
		}
		return []network.NextHop{{Gateway: target.String(), Interface: hint, Weight: 1}}, nil
	}
	seen[key] = true
	defer delete(seen, key)
	var candidates []fibCandidate
	for bits := target.BitLen(); bits >= 0; bits-- {
		if found := i[table][netip.PrefixFrom(target, bits).Masked()]; len(found) > 0 {
			candidates = found
			break
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("next hop %s has no committed route in table %d", target, table)
	}
	best := int(^uint(0) >> 1)
	for _, c := range candidates {
		if c.metric < best {
			best = c.metric
		}
	}
	var result []network.NextHop
	var last error
	for _, c := range candidates {
		if c.metric != best {
			continue
		}
		for _, hop := range c.hops {
			if hint != "" && hop.Interface != "" && hint != hop.Interface {
				continue
			}
			if hop.Gateway == "" {
				if hop.Interface == "" {
					continue
				}
				result = append(result, network.NextHop{Gateway: target.String(), Interface: hop.Interface, Weight: hop.Weight})
				continue
			}
			next, err := netip.ParseAddr(hop.Gateway)
			if err != nil {
				last = err
				continue
			}
			resolved, err := i.resolve(next, table, hop.Interface, seen, depth+1)
			if err != nil {
				last = err
				continue
			}
			result = append(result, resolved...)
		}
	}
	if len(result) == 0 {
		if last == nil {
			last = fmt.Errorf("next hop %s is unreachable on configured interface %s", target, hint)
		}
		return nil, last
	}
	result = uniqueHops(result)
	if len(result) > 64 {
		return nil, fmt.Errorf("next hop %s expands beyond 64 paths", target)
	}
	return result, nil
}
func uniqueHops(hops []network.NextHop) []network.NextHop {
	byKey := map[string]network.NextHop{}
	for _, h := range hops {
		if h.Weight == 0 {
			h.Weight = 1
		}
		key := h.Interface + "/" + h.Gateway + "/" + strconv.Itoa(h.Weight)
		byKey[key] = h
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]network.NextHop, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out
}
