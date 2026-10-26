package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
)

func (e *Engine) observationHeaderLocked() (ObservationHeader, error) {
	id, err := observationDigest(struct {
		Revision, AttemptedRevision uint64
		Status                      string
		Desired                     Desired
		Attempted                   *Desired
	}{e.state.Revision, e.state.AttemptedRevision, e.state.Status, e.state.Desired, e.state.Attempted})
	return ObservationHeader{Revision: e.state.Revision, AttemptedRevision: e.state.AttemptedRevision, Status: observationText(e.state.Status, 64), ObservedAt: time.Now().UTC(), ProjectionID: id}, err
}

func (e *Engine) observationReadyLocked(ctx context.Context) error {
	if e.lock == nil {
		return errors.New("network engine is closed")
	}
	return ctx.Err()
}

// Observe is the bounded operator view. Observation failures are represented
// explicitly in Complete/ObservationErrors; protocol dependencies use their own
// minimal query and never infer absence from a preview or incomplete view.
func (e *Engine) Observe(ctx context.Context) (ObservationView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.observationReadyLocked(ctx); err != nil {
		return ObservationView{}, err
	}
	h, err := e.observationHeaderLocked()
	if err != nil {
		return ObservationView{}, err
	}
	s := e.observationSnapshotLocked(ctx)
	h.ObservedAt = time.Now().UTC()
	return boundedSnapshot(s, h)
}

// Use indexed ownership for operator observations. The legacy snapshot path
// compares each observed route against every managed route; at large FIB sizes
// that can hold the engine lock long enough to delay minimal protocol queries.
// This remains the same scoped, per-read kernel observation, not a new source
// of ownership: each indexed candidate still passes the exact existing check.
func (e *Engine) observationRouteOwnershipLocked() func(RouteState) bool {
	type key struct {
		destination   string
		table, metric int
	}
	index := make(map[key][]ownedRoute, len(e.state.Routes))
	for _, route := range e.state.Routes {
		route.Route = normalizeRoute(route.Route)
		k := key{route.Destination, route.Table, route.Metric}
		index[k] = append(index[k], route)
	}
	return func(route RouteState) bool {
		route.Route = normalizeRoute(route.Route)
		for _, candidate := range index[key{route.Destination, route.Table, route.Metric}] {
			if routeMatches(route, candidate) {
				return true
			}
		}
		return false
	}
}

func (e *Engine) observationSnapshotLocked(ctx context.Context) Snapshot {
	s := Snapshot{Revision: e.state.Revision, AttemptedRevision: e.state.AttemptedRevision, Status: e.state.Status, Desired: cloneDesired(e.state.Desired), LastError: e.state.LastError, Firewall: FirewallState{Table: FirewallTable}}
	problem := func(err error) {
		if err != nil {
			s.ObservationErrors = append(s.ObservationErrors, err.Error())
		}
	}
	names, tables := e.observationTargetsLocked()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			problem(err)
			return s
		}
		l, err := e.observeInterfaceLocked(name)
		if err != nil {
			problem(fmt.Errorf("interface %s: %w", name, err))
			continue
		}
		s.Interfaces = append(s.Interfaces, l)
		if e.wireGuardRecord(name) != nil && l.Kind == "wireguard" {
			device, err := e.observeWireGuardLocked(name)
			problem(err)
			if err == nil {
				s.WireGuard = append(s.WireGuard, device)
			}
		}
	}
	ownsRoute := e.observationRouteOwnershipLocked()
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			problem(err)
			return s
		}
		routes, err := e.kernel.Routes(table)
		problem(err)
		for _, route := range routes {
			route.Owned = ownsRoute(route)
			s.Routes = append(s.Routes, route)
		}
	}
	if len(e.state.PolicyRules) > 0 || len(e.state.Desired.PolicyRules) > 0 || e.state.Attempted != nil && len(e.state.Attempted.PolicyRules) > 0 {
		rules, err := e.kernel.PolicyRules()
		problem(err)
		for _, rule := range rules {
			rule.Owned = e.ownsPolicyRule(rule)
			s.PolicyRules = append(s.PolicyRules, rule)
		}
	}
	v, err := e.kernel.ReadSetting(settingKey{Kind: "forwarding4"})
	problem(err)
	s.Forwarding.IPv4 = v == 1
	v, err = e.kernel.ReadSetting(settingKey{Kind: "forwarding6"})
	problem(err)
	s.Forwarding.IPv6 = v == 1
	fw, err := e.kernel.Firewall()
	problem(err)
	if err == nil {
		fw.Owned = fw.Owned && e.state.FirewallOwned
		s.Firewall = fw
	}
	guard, err := e.kernel.VRRPGuard()
	problem(err)
	if err == nil {
		guard.Owned = guard.Owned && e.state.VRRPGuardOwned
		s.VRRPGuard = guard
	}
	return s
}

// observationTargetsLocked uses the same referenced-interface/table scope as
// Snapshot. An explicit table query can also inspect another table without
// claiming ownership or including it in the engine's configuration.
func (e *Engine) observationTargetsLocked() ([]string, []int) {
	names, tables := map[string]bool{}, map[int]bool{}
	collect := func(d Desired) {
		for _, w := range d.WireGuard {
			names[w.Interface] = true
		}
		for _, n := range firewallInterfaces(d.Firewall) {
			names[n] = true
		}
		for _, r := range d.PolicyRules {
			tables[r.Table] = true
			if r.InInterface != "" {
				names[r.InInterface] = true
			}
			if r.OutInterface != "" {
				names[r.OutInterface] = true
			}
		}
		for _, l := range d.Links {
			names[l.Name] = true
			if l.Parent != "" {
				names[l.Parent] = true
			}
		}
		for _, l := range d.Interfaces {
			names[l.Name] = true
			if l.Master != nil && *l.Master != "" {
				names[*l.Master] = true
			}
		}
		for _, r := range d.Routes {
			if r.Interface != "" {
				names[r.Interface] = true
			}
			for _, hop := range r.NextHops {
				names[hop.Interface] = true
			}
			tables[routeTable(r)] = true
		}
	}
	collect(e.state.Desired)
	if e.state.Attempted != nil {
		collect(*e.state.Attempted)
	}
	for _, a := range e.state.Addresses {
		names[a.Interface] = true
	}
	for _, l := range e.state.Links {
		names[l.Definition.Name] = true
	}
	for _, a := range e.state.Attributes {
		names[a.Key.Interface] = true
	}
	for _, r := range e.state.Routes {
		tables[routeTable(r.Route)] = true
	}
	for _, r := range e.state.PolicyRules {
		tables[r.Table] = true
	}
	ordered, tableIDs := make([]string, 0, len(names)), make([]int, 0, len(tables))
	for name := range names {
		ordered = append(ordered, name)
	}
	for table := range tables {
		tableIDs = append(tableIDs, table)
	}
	sort.Strings(ordered)
	sort.Ints(tableIDs)
	return ordered, tableIDs
}

func (e *Engine) observeInterfaceLocked(name string) (InterfaceState, error) {
	l, err := e.kernel.Interface(name)
	if err != nil {
		return l, err
	}
	l.Owned = e.ownsLink(l)
	l.Addresses = append([]AddressState(nil), l.Addresses...)
	records := map[string]*ownedAddress{}
	for i := range e.state.Addresses {
		record := &e.state.Addresses[i]
		if record.Interface == name {
			records[record.Address] = record
		}
	}
	for i := range l.Addresses {
		a := &l.Addresses[i]
		a.Owned = observationAddressOwned(*a, l.Index, records[a.Address])
	}
	sort.Slice(l.Addresses, func(i, j int) bool { return l.Addresses[i].Address < l.Addresses[j].Address })
	return l, nil
}

func observationAddressOwned(a AddressState, index int, record *ownedAddress) bool {
	now := time.Now()
	return record != nil && (record.Index == 0 || record.Index == index) && (addressOptionsMatch(a, record.Options, now) || record.Attempted != nil && addressOptionsMatch(a, *record.Attempted, now))
}

func (e *Engine) observeWireGuardLocked(name string) (WireGuardState, error) {
	record := e.wireGuardRecord(name)
	if record == nil {
		return WireGuardState{}, ErrObservationNotFound
	}
	l, err := e.kernel.Interface(name)
	if err != nil {
		return WireGuardState{}, err
	}
	if l.Kind != "wireguard" {
		return WireGuardState{}, ErrObservationNotFound
	}
	k, ok := e.kernel.(wireGuardKernel)
	if !ok {
		return WireGuardState{}, errors.New("native WireGuard observation is unavailable")
	}
	live, err := k.ReadWireGuard(l.Index)
	defer clearWireGuardDevice(&live)
	if err != nil {
		return WireGuardState{}, err
	}
	return publicWireGuardState(name, live, e.ownsLink(l) && wireGuardOwned(live, *record), record.Quarantined), nil
}

func routeObservationID(r RouteState) (string, error) {
	r.Route = normalizeRoute(r.Route)
	// Remaining lifetime changes between kernel reads; the endpoint identifies
	// a routing entry, not a particular timer tick or ownership observation.
	r.ValidUntil, r.Owned = lifetime.Deadline{}, false
	return observationDigest(r)
}

type observationRoute struct {
	route RouteState
	id    string
}

func (e *Engine) observeRoutesLocked(table int) ([]observationRoute, error) {
	tables := []int{table}
	if table == 0 {
		_, tables = e.observationTargetsLocked()
	}
	var out []observationRoute
	ownsRoute := e.observationRouteOwnershipLocked()
	for _, selected := range tables {
		routes, err := e.kernel.Routes(selected)
		if err != nil {
			return nil, err
		}
		for _, route := range routes {
			route.Owned = ownsRoute(route)
			id, err := routeObservationID(route)
			if err != nil {
				return nil, err
			}
			out = append(out, observationRoute{route, id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].route, out[j].route
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		return out[i].id < out[j].id
	})
	return out, nil
}

func (e *Engine) ObservationPage(ctx context.Context, q ObservationQuery) (page ObservationPage, err error) {
	if err := q.Validate(); err != nil {
		return page, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.observationReadyLocked(ctx); err != nil {
		return page, err
	}
	h, err := e.observationHeaderLocked()
	if err != nil {
		return page, err
	}
	if q.ProjectionID != "" && q.ProjectionID != h.ProjectionID {
		return page, ErrObservationChanged
	}
	defer func() { page.ObservedAt = time.Now().UTC() }()
	switch q.Section {
	case "interfaces":
		names, _ := e.observationTargetsLocked()
		return makeObservationPage(h, q, len(names), func(i int) (any, error) {
			l, err := e.observeInterfaceLocked(names[i])
			if err != nil {
				return nil, err
			}
			v := InterfaceObservation{InterfaceState: l, AddressCount: len(l.Addresses), AddressesTruncated: len(l.Addresses) > 8}
			v.Addresses = v.Addresses[:min(8, len(v.Addresses))]
			return v, nil
		})
	case "addresses":
		l, err := e.observeInterfaceLocked(q.Interface)
		if err != nil {
			return page, err
		}
		return makeObservationPage(h, q, len(l.Addresses), func(i int) (any, error) { return l.Addresses[i], nil })
	case "routes", "route-next-hops":
		routes, err := e.observeRoutesLocked(q.Table)
		if err != nil {
			return page, err
		}
		if q.Section == "route-next-hops" {
			for _, route := range routes {
				if route.id != q.RouteID {
					continue
				}
				hops := route.route.NextHops
				return makeObservationPage(h, q, len(hops), func(i int) (any, error) { return hops[i], nil })
			}
			return page, ErrObservationNotFound
		}
		return makeObservationPage(h, q, len(routes), func(i int) (any, error) {
			r := routes[i]
			v := RouteObservation{RouteState: r.route, RouteID: r.id, NextHopCount: len(r.route.NextHops), NextHopsTruncated: len(r.route.NextHops) > 8}
			v.NextHops = v.NextHops[:min(8, len(v.NextHops))]
			return v, nil
		})
	case "policy-rules":
		rules, err := e.kernel.PolicyRules()
		if err != nil {
			return page, err
		}
		for i := range rules {
			rules[i].Owned = e.ownsPolicyRule(rules[i])
		}
		sort.Slice(rules, func(i, j int) bool {
			if rules[i].Family != rules[j].Family {
				return rules[i].Family < rules[j].Family
			}
			if rules[i].Priority != rules[j].Priority {
				return rules[i].Priority < rules[j].Priority
			}
			a, _ := json.Marshal(rules[i])
			b, _ := json.Marshal(rules[j])
			return string(a) < string(b)
		})
		return makeObservationPage(h, q, len(rules), func(i int) (any, error) { return rules[i], nil })
	case "wireguard":
		names := make([]string, 0, len(e.state.WireGuard))
		for _, record := range e.state.WireGuard {
			names = append(names, record.Interface)
		}
		sort.Strings(names)
		return makeObservationPage(h, q, len(names), func(i int) (any, error) {
			d, err := e.observeWireGuardLocked(names[i])
			if err != nil {
				return nil, err
			}
			v := WireGuardObservation{WireGuardState: d, PeerCount: len(d.Peers), PeersTruncated: len(d.Peers) != 0}
			v.Peers = nil // Peer details are a separate bounded collection.
			return v, nil
		})
	case "wireguard-peers", "wireguard-allowed-ips":
		d, err := e.observeWireGuardLocked(q.Interface)
		if err != nil {
			return page, err
		}
		if q.Section == "wireguard-allowed-ips" {
			for _, peer := range d.Peers {
				if peer.PublicKey != q.PublicKey {
					continue
				}
				return makeObservationPage(h, q, len(peer.AllowedIPs), func(i int) (any, error) { return peer.AllowedIPs[i], nil })
			}
			return page, ErrObservationNotFound
		}
		return makeObservationPage(h, q, len(d.Peers), func(i int) (any, error) {
			p := d.Peers[i]
			v := WireGuardPeerObservation{WireGuardPeerState: p, AllowedIPCount: len(p.AllowedIPs), AllowedIPsTruncated: len(p.AllowedIPs) > 8}
			v.AllowedIPs = v.AllowedIPs[:min(8, len(v.AllowedIPs))]
			return v, nil
		})
	}
	return page, errors.New("unsupported network observation section")
}

func (e *Engine) DesiredObservation(ctx context.Context, projectionID string, offset, limit int) (DesiredPage, error) {
	if err := ValidateDesiredObservationQuery(projectionID, offset, limit); err != nil {
		return DesiredPage{}, err
	}
	if limit == 0 {
		limit = DesiredObservationChunk
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.observationReadyLocked(ctx); err != nil {
		return DesiredPage{}, err
	}
	h, err := e.observationHeaderLocked()
	if err != nil {
		return DesiredPage{}, err
	}
	if projectionID != "" && projectionID != h.ProjectionID {
		return DesiredPage{}, ErrObservationChanged
	}
	encoded, err := json.Marshal(e.state.Desired)
	if err != nil {
		return DesiredPage{}, err
	}
	if len(encoded) > MaxDesiredObservationBytes {
		return DesiredPage{}, ErrObservationTooLarge
	}
	start := min(offset, len(encoded))
	end := min(start+limit, len(encoded))
	page := DesiredPage{ObservationHeader: h, TotalBytes: len(encoded), Offset: offset, Data: encoded[start:end]}
	if end < len(encoded) {
		page.NextOffset = &end
	}
	return page, nil
}

func (e *Engine) SourceAddress(ctx context.Context, name, address string) (SourceObservation, error) {
	ip, err := validateSourceObservation(name, address)
	if err != nil {
		return SourceObservation{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.observationReadyLocked(ctx); err != nil {
		return SourceObservation{}, err
	}
	// Do not hash/encode the full Desired just to authorize one source address.
	// Protocol inputs already carry their own configuration digest/generation.
	out := SourceObservation{ObservationHeader: ObservationHeader{Revision: e.state.Revision, AttemptedRevision: e.state.AttemptedRevision, Status: observationText(e.state.Status, 64), ObservedAt: time.Now().UTC()}, Interface: name, RequestedAddress: ip.String()}
	l, err := e.kernel.Interface(name)
	if errors.Is(err, errLinkNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.InterfacePresent, out.Index, out.Up = true, l.Index, l.Up
	for _, a := range l.Addresses {
		p, err := netip.ParsePrefix(a.Address)
		if err != nil || p.Addr() != ip {
			continue
		}
		a.Owned = observationAddressOwned(a, l.Index, e.addressRecord(addressKey{name, a.Address}))
		if out.Address == nil || sourceAddressUsable(a) && !sourceAddressUsable(*out.Address) {
			copy := a
			out.Address, out.Found = &copy, true
		}
	}
	out.ObservedAt = time.Now().UTC()
	return out, nil
}

func sourceAddressUsable(address AddressState) bool {
	// Linux IFA_F_DADFAILED=0x08, DEPRECATED=0x20, TENTATIVE=0x40. Keeping the
	// constants here platform-neutral also keeps the fake engine portable.
	return address.Owned && address.PreferredLifetime > 0 && address.ValidLifetime > 0 && address.Flags&(0x08|0x20|0x40) == 0
}
