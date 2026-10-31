package runtimeplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
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
	bootID        string
	expiresNanos  uint64
}
type fibCandidate struct {
	prefix        netip.Prefix
	table, metric int
	hops          []network.NextHop
	expiresNanos  uint64 // zero means no finite prerequisite
}
type fibIndex struct {
	tables map[int]map[netip.Prefix][]fibCandidate
	links  map[string]uint64
}

func (i *fibIndex) add(c fibCandidate) {
	if i.tables == nil {
		i.tables = map[int]map[netip.Prefix][]fibCandidate{}
	}
	if i.tables[c.table] == nil {
		i.tables[c.table] = map[netip.Prefix][]fibCandidate{}
	}
	i.tables[c.table][c.prefix] = append(i.tables[c.table][c.prefix], c)
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
func projectBGP(p *projection, doc config.Native, configs map[string]bgpconfig.Config, records map[string]serviceconfig.Update, now time.Time, clock bfd.Clock) error {
	p.bgpStatus = map[string]bgpProjectionStatus{}
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
		if !matchesBinding(p.instances, service.ModuleName(), u) {
			continue
		}
		var s bgpconfig.Snapshot
		if err := json.Unmarshal(u.Snapshot, &s); err != nil {
			return err
		}
		if !bgpconfig.CurrentObservation(s, clock) {
			continue
		}
		provider, err := bgpProviderFor(u)
		if err != nil {
			return err
		}
		p.bgpStatus[service.ModuleName()] = bgpProjectionStatus{identity: provider.projection}
		c := configs[service.ModuleName()]
		if len(s.Routes) > c.MaxRoutes {
			return errors.New("BGP selected routes exceed configured limit")
		}
		for _, route := range s.Routes {
			until := s.ExpiresNanos
			if ref, required := c.PeerBFD[route.PeerAddress]; required {
				if ref.Address == "" {
					ref.Address = route.PeerAddress
				}
				grant := sources[ref.Service].grant(ref, clock)
				if !acknowledgedBFD(grant, s.BFD[route.PeerAddress]) {
					// Independently withdraw the FIB path while the BGP worker
					// closes its session. The route's own ownership heartbeat
					// cannot extend a BFD receive deadline.
					continue
				}
				until = min(until, uint64(grant.DeadlineBootNanoseconds))
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
				group = &routeGroup{service: service.Name, prefix: prefix, table: c.Table, metric: c.RouteMetric, bootID: s.BootID, expiresNanos: until}
				groups[key] = group
			} else if group.service != service.Name {
				return fmt.Errorf("BGP instances %s and %s compete for prefix/table/metric %s", group.service, service.Name, key)
			}
			group.expiresNanos = min(group.expiresNanos, until)
			group.paths = append(group.paths, route)
		}
	}
	// Resolve against the same finite prerequisites that authorize the route.
	// Flattening a recursive next hop must not discard its original expiry.
	deadline := func(boot string, nanos uint64, wall time.Time) uint64 {
		if boot != "" {
			if boot != clock.BootID {
				return uint64(clock.BoottimeNanoseconds)
			}
			if nanos == 0 {
				return math.MaxUint64
			}
			return nanos
		}
		if wall.IsZero() {
			return math.MaxUint64
		}
		return uint64(clock.BoottimeNanoseconds) + uint64(max(0, wall.Sub(now)))
	}
	index := fibIndex{links: map[string]uint64{}}
	domains := network.RoutingDomains(p.network)
	for _, l := range p.network.Links {
		if l.Authority != (lifetime.Deadline{}) {
			index.links[l.Name] = deadline(l.Authority.BootID, l.Authority.Nanos, l.Authority.UTC)
		}
	}
	for _, l := range p.network.Interfaces {
		table := domains[l.Name].Table
		for _, value := range l.Addresses {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return err
			}
			options := l.AddressOptions[value]
			until := deadline(options.BootLifetime.BootID, options.BootLifetime.ValidNanos, options.ValidUntil)
			if linkUntil := index.links[l.Name]; linkUntil != 0 {
				until = min(until, linkUntil)
			}
			if until > uint64(clock.BoottimeNanoseconds) {
				index.add(fibCandidate{prefix: prefix.Masked(), table: table, expiresNanos: until, hops: []network.NextHop{{Interface: l.Name, Weight: 1}}})
			}
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
		until := min(deadline(r.BootID, r.ExpiresNanos, time.Time{}), deadline(r.ValidUntil.BootID, r.ValidUntil.Nanos, r.ValidUntil.UTC))
		for _, hop := range hops {
			if linkUntil := index.links[hop.Interface]; linkUntil != 0 {
				until = min(until, linkUntil)
			}
		}
		if until > uint64(clock.BoottimeNanoseconds) {
			index.add(fibCandidate{prefix: prefix, table: routeTable(r.Table), metric: routeMetric(r.Metric, prefix.Addr()), hops: hops, expiresNanos: until})
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := groups[key]
		candidate := fibCandidate{prefix: g.prefix, table: g.table, metric: g.metric, expiresNanos: g.expiresNanos}
		for _, path := range g.paths {
			candidate.hops = append(candidate.hops, network.NextHop{Gateway: path.NextHop, Interface: path.Interface, Weight: 1})
		}
		index.add(candidate)
	}
	for _, key := range keys {
		g := groups[key]
		var next []network.NextHop
		var reasons []string
		until := g.expiresNanos
		for _, path := range g.paths {
			hops, resolvedUntil, err := index.resolve(netip.MustParseAddr(path.NextHop), g.table, path.Interface, map[string]bool{}, 0)
			if err != nil {
				reasons = append(reasons, err.Error())
				continue
			}
			if resolvedUntil <= uint64(clock.BoottimeNanoseconds) {
				reasons = append(reasons, fmt.Sprintf("next hop %s prerequisite expired", path.NextHop))
				continue
			}
			next = append(next, hops...)
			until = min(until, resolvedUntil)
		}
		if len(reasons) > 0 {
			p.routeIssues = append(p.routeIssues, RouteIssue{Service: g.service, Prefix: g.prefix.String(), Reason: strings.Join(unique(reasons), "; ")})
			status := p.bgpStatus["service."+g.service]
			status.unresolved++
			p.bgpStatus["service."+g.service] = status
		}
		next = uniqueHops(next)
		if len(next) == 0 {
			continue
		}
		route := network.Route{Destination: g.prefix.String(), Table: g.table, Metric: g.metric, BootID: g.bootID, ExpiresNanos: until}
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
func (i fibIndex) resolve(target netip.Addr, table int, hint string, seen map[string]bool, depth int) ([]network.NextHop, uint64, error) {
	key := fmt.Sprintf("%d:%s%%%s", table, target, hint)
	if seen[key] || depth > 16 {
		return nil, 0, fmt.Errorf("next-hop resolution loop for %s in table %d", target, table)
	}
	if target.IsLinkLocalUnicast() {
		if hint == "" {
			return nil, 0, fmt.Errorf("link-local next hop %s needs an explicit interface", target)
		}
		until := uint64(math.MaxUint64)
		if linkUntil := i.links[hint]; linkUntil != 0 {
			until = linkUntil
		}
		return []network.NextHop{{Gateway: target.String(), Interface: hint, Weight: 1}}, until, nil
	}
	seen[key] = true
	defer delete(seen, key)
	var candidates []fibCandidate
	for bits := target.BitLen(); bits >= 0; bits-- {
		if found := i.tables[table][netip.PrefixFrom(target, bits).Masked()]; len(found) > 0 {
			candidates = found
			break
		}
	}
	if len(candidates) == 0 {
		return nil, 0, fmt.Errorf("next hop %s has no committed route in table %d", target, table)
	}
	best := int(^uint(0) >> 1)
	for _, c := range candidates {
		if c.metric < best {
			best = c.metric
		}
	}
	var result []network.NextHop
	until := uint64(math.MaxUint64)
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
				if c.expiresNanos != 0 {
					until = min(until, c.expiresNanos)
				}
				continue
			}
			next, err := netip.ParseAddr(hop.Gateway)
			if err != nil {
				last = err
				continue
			}
			resolved, resolvedUntil, err := i.resolve(next, table, hop.Interface, seen, depth+1)
			if err != nil {
				last = err
				continue
			}
			result = append(result, resolved...)
			until = min(until, resolvedUntil)
			if c.expiresNanos != 0 {
				until = min(until, c.expiresNanos)
			}
		}
	}
	if len(result) == 0 {
		if last == nil {
			last = fmt.Errorf("next hop %s is unreachable on configured interface %s", target, hint)
		}
		return nil, 0, last
	}
	result = uniqueHops(result)
	if len(result) > 64 {
		return nil, 0, fmt.Errorf("next hop %s expands beyond 64 paths", target)
	}
	return result, until, nil
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
