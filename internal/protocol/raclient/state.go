// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"sort"
	"time"
)

// All internal deadlines use a synthetic CLOCK_BOOTTIME epoch. Public times
// use wall UTC. This makes NTP steps and suspend irrelevant to live lifetimes.
type instant struct{ wall, boot time.Time }
type linkInfo struct {
	index, mtu int
	hardware   net.HardwareAddr
	source     netip.Addr
}
type state struct {
	config  Config
	link    linkInfo
	iid     [8]byte
	learned Snapshot
	clock   instant // stable wall/BOOTTIME mapping, preserved by the journal pair
}

func newState(c Config, link linkInfo) (*state, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	if link.index <= 0 || !link.source.IsLinkLocalUnicast() || link.source.Zone() != "" || link.mtu < 1280 {
		return nil, errors.New("RA client requires a usable IPv6 link and link-local source")
	}
	var iid [8]byte
	if c.InterfaceID != "" {
		var err error
		iid, err = interfaceID(c.InterfaceID)
		if err != nil {
			return nil, err
		}
	} else {
		if len(link.hardware) != 6 || link.hardware[0]&1 != 0 {
			return nil, errors.New("RA client needs an explicit interface_id on non-Ethernet or multicast hardware identities")
		}
		iid = [8]byte{link.hardware[0] ^ 2, link.hardware[1], link.hardware[2], 0xff, 0xfe, link.hardware[3], link.hardware[4], link.hardware[5]}
	}
	return &state{config: c, link: link, iid: iid, learned: Snapshot{Resource: c.Resource, Interface: c.Interface, SourceAddress: link.source.String(), InterfaceID: suffixString(iid), Active: true}}, nil
}

func clone(s Snapshot) Snapshot {
	s.Addresses = slices.Clone(s.Addresses)
	s.Prefixes = slices.Clone(s.Prefixes)
	s.Routes = slices.Clone(s.Routes)
	s.DNSServers = slices.Clone(s.DNSServers)
	s.SearchDomains = slices.Clone(s.SearchDomains)
	return s
}

func convertDeadline(t, from, to time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return to.Add(t.Sub(from)).UTC()
}

func convertDeadlines(s *Snapshot, from, to time.Time) {
	for i := range s.Addresses {
		s.Addresses[i].ValidUntil = convertDeadline(s.Addresses[i].ValidUntil, from, to)
		s.Addresses[i].PreferredUntil = convertDeadline(s.Addresses[i].PreferredUntil, from, to)
	}
	for i := range s.Prefixes {
		s.Prefixes[i].ValidUntil = convertDeadline(s.Prefixes[i].ValidUntil, from, to)
	}
	for i := range s.Routes {
		s.Routes[i].ValidUntil = convertDeadline(s.Routes[i].ValidUntil, from, to)
	}
	for i := range s.DNSServers {
		s.DNSServers[i].ValidUntil = convertDeadline(s.DNSServers[i].ValidUntil, from, to)
	}
	for i := range s.SearchDomains {
		s.SearchDomains[i].ValidUntil = convertDeadline(s.SearchDomains[i].ValidUntil, from, to)
	}
}

func (s *state) snapshot(now instant) Snapshot {
	now = s.stableClock(now)
	out := clone(s.learned)
	convertDeadlines(&out, now.boot, now.wall)
	out.UpdatedAt = now.wall.UTC()
	return out
}

// UTC lifetime facts must remain exactly constant when only status counters
// change. Separate wall/BOOTTIME reads have sampling jitter; only a material
// offset change replaces this mapping. UpdatedAt uses the same mapping so the
// durable journal can convert every deadline back to its original boot time.
func (s *state) stableClock(now instant) instant {
	if s.clock.boot.IsZero() || wallClockMoved(s.clock, now) {
		s.clock = now
	}
	now.wall = convertDeadline(now.boot, s.clock.boot, s.clock.wall)
	return now
}

func wallClockMoved(before, after instant) bool {
	shift := after.wall.Sub(before.wall) - after.boot.Sub(before.boot)
	return shift >= time.Second || shift <= -time.Second
}

func expiry(now time.Time, seconds uint32) time.Time {
	if seconds == ^uint32(0) {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds) * time.Second)
}
func expired(until, now time.Time) bool { return !until.IsZero() && !now.Before(until) }

// update is deliberately generic only over slice storage; protocol-specific
// key/lifetime semantics stay in handle below.
func update[T any](items []T, matches func(T) bool, value T, remove bool) []T {
	for i, item := range items {
		if matches(item) {
			if remove {
				return append(items[:i], items[i+1:]...)
			}
			items[i] = value
			return items
		}
	}
	if !remove {
		return append(items, value)
	}
	return items
}

func expireSnapshot(s *Snapshot, now time.Time) bool {
	changed := false
	before := len(s.Addresses) + len(s.Prefixes) + len(s.Routes) + len(s.DNSServers) + len(s.SearchDomains)
	s.Addresses = slices.DeleteFunc(s.Addresses, func(a Address) bool { return expired(a.ValidUntil, now) })
	s.Prefixes = slices.DeleteFunc(s.Prefixes, func(p Prefix) bool { return expired(p.ValidUntil, now) })
	s.Routes = slices.DeleteFunc(s.Routes, func(r Route) bool { return expired(r.ValidUntil, now) })
	s.DNSServers = slices.DeleteFunc(s.DNSServers, func(d DNSServer) bool { return expired(d.ValidUntil, now) })
	s.SearchDomains = slices.DeleteFunc(s.SearchDomains, func(d SearchDomain) bool { return expired(d.ValidUntil, now) })
	for i := range s.Addresses {
		a := &s.Addresses[i]
		deprecated := expired(a.PreferredUntil, now)
		if a.Deprecated != deprecated {
			a.Deprecated = deprecated
			changed = true
		}
	}
	return changed || before != len(s.Addresses)+len(s.Prefixes)+len(s.Routes)+len(s.DNSServers)+len(s.SearchDomains)
}

func (s *state) expire(now instant) bool { return expireSnapshot(&s.learned, now.boot) }

// validLifetime implements RFC 4862 5.5.3(e). A whitelist does not authenticate
// an RA, so it never bypasses the two-hour safeguard. Preferred lifetime still
// changes immediately, including immediate deprecation on a zero-lifetime PIO.
func validLifetime(old, now time.Time, advertised uint32) time.Time {
	if advertised == ^uint32(0) {
		return time.Time{}
	}
	next := time.Duration(advertised) * time.Second
	remaining := old.Sub(now)
	if old.IsZero() {
		remaining = time.Duration(1<<63 - 1)
	}
	if next > 2*time.Hour || next > remaining {
		return now.Add(next)
	}
	if remaining <= 2*time.Hour {
		return old
	}
	return now.Add(2 * time.Hour)
}

func (s *state) handle(p packet, now instant) error {
	a, err := parse(p, s.link.index, s.link.mtu)
	if err == nil && (p.destination.IsMulticast() && p.destination != netip.MustParseAddr("ff02::1") || !p.destination.IsMulticast() && p.destination != s.link.source) {
		err = snapshotError("unexpected destination")
	}
	if err == nil && !allowed(s.config, a.router.String()) {
		err = snapshotError("router outside configured allowlist")
	}
	if err != nil {
		s.learned.Rejected++
		s.learned.LastError = err.Error()
		return err
	}
	next := clone(s.learned)
	expireSnapshot(&next, now.boot)
	next.LastReceivedAt, next.LastRouter = now.wall.UTC(), a.router.String()
	next.Managed, next.OtherConfig = a.managed, a.other
	if a.mtu != 0 {
		next.MTU = a.mtu
	}
	if a.hopLimit != 0 {
		next.CurrentHopLimit = a.hopLimit
	}
	if a.reachable != 0 && a.reachable <= 3600000 {
		next.ReachableMilliseconds = a.reachable
	}
	if a.retransmit != 0 {
		next.RetransmitMilliseconds = a.retransmit
	}
	putRoute := func(prefix netip.Prefix, pref int, lifetime uint32) {
		r := Route{Destination: prefix.String(), Gateway: a.router.String(), Preference: pref, ValidUntil: expiry(now.boot, lifetime)}
		next.Routes = update(next.Routes, func(old Route) bool { return old.Destination == r.Destination && old.Gateway == r.Gateway }, r, lifetime == 0)
	}
	if enabled(s.config.DefaultRoute) {
		putRoute(netip.MustParsePrefix("::/0"), a.preference, a.lifetime)
	}
	if enabled(s.config.RouteInformation) {
		for _, r := range a.routes {
			if r.prefix.Bits() == 0 && !enabled(s.config.DefaultRoute) {
				continue
			}
			putRoute(r.prefix, r.preference, r.lifetime)
		}
	}
	for _, p := range a.prefixes {
		if p.onLink && enabled(s.config.OnLink) {
			v := Prefix{Prefix: p.prefix.String(), ValidUntil: expiry(now.boot, p.valid)}
			next.Prefixes = update(next.Prefixes, func(old Prefix) bool { return old.Prefix == v.Prefix }, v, p.valid == 0)
		}
		if !p.autonomous || !enabled(s.config.SLAAC) || p.prefix.Bits() != 64 {
			continue
		}
		raw := p.prefix.Addr().As16()
		copy(raw[8:], s.iid[:])
		address := netip.PrefixFrom(netip.AddrFrom16(raw), 128).String()
		var old *Address
		for i := range next.Addresses {
			if next.Addresses[i].Address == address {
				old = &next.Addresses[i]
				break
			}
		}
		if old == nil && p.valid == 0 {
			continue
		}
		v := Address{Address: address, Prefix: p.prefix.String(), ValidUntil: expiry(now.boot, p.valid), PreferredUntil: expiry(now.boot, p.preferred), Deprecated: p.preferred == 0}
		if old != nil {
			v.ValidUntil = validLifetime(old.ValidUntil, now.boot, p.valid)
			v.DADFailed = old.DADFailed
		}
		next.Addresses = update(next.Addresses, func(old Address) bool { return old.Address == address }, v, false)
	}
	if enabled(s.config.DNS) {
		newServers, newDomains := []string{}, []string{}
		for _, d := range a.dns {
			for _, address := range d.addresses {
				v := DNSServer{Address: address.String(), Router: a.router.String(), ValidUntil: expiry(now.boot, d.lifetime)}
				if d.lifetime != 0 && !slices.ContainsFunc(next.DNSServers, func(old DNSServer) bool { return old.Address == v.Address }) && !slices.Contains(newServers, v.Address) {
					newServers = append(newServers, v.Address)
				}
				next.DNSServers = update(next.DNSServers, func(old DNSServer) bool { return old.Address == v.Address }, v, d.lifetime == 0)
			}
			for _, domain := range d.domains {
				v := SearchDomain{Domain: domain, Router: a.router.String(), ValidUntil: expiry(now.boot, d.lifetime)}
				if d.lifetime != 0 && !slices.ContainsFunc(next.SearchDomains, func(old SearchDomain) bool { return old.Domain == v.Domain }) && !slices.Contains(newDomains, v.Domain) {
					newDomains = append(newDomains, v.Domain)
				}
				next.SearchDomains = update(next.SearchDomains, func(old SearchDomain) bool { return old.Domain == v.Domain }, v, d.lifetime == 0)
			}
		}
		// RFC 8106 section 6: refresh existing entries in place, and put new
		// entries at the front in wire order rather than reversing each option.
		promoteDNS(next.DNSServers, newServers, func(v DNSServer) string { return v.Address })
		promoteDNS(next.SearchDomains, newDomains, func(v SearchDomain) string { return v.Domain })
	}
	if len(next.Addresses) > MaxAddresses || len(next.Prefixes) > MaxPrefixes || len(next.Routes) > MaxRoutes || len(next.DNSServers) > MaxDNSServers || len(next.SearchDomains) > MaxSearchDomains {
		err := snapshotError("learned-state capacity exceeded; advertisement rejected atomically")
		s.learned.Rejected++
		s.learned.LastError = err.Error()
		return err
	}
	// Address/route ordering is canonical; DNS ordering follows its repository.
	sort.Slice(next.Addresses, func(i, j int) bool { return next.Addresses[i].Address < next.Addresses[j].Address })
	sort.Slice(next.Prefixes, func(i, j int) bool { return next.Prefixes[i].Prefix < next.Prefixes[j].Prefix })
	sort.Slice(next.Routes, func(i, j int) bool {
		a, b := next.Routes[i], next.Routes[j]
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.Gateway < b.Gateway
	})
	next.Received++
	next.LastError = ""
	for _, address := range next.Addresses {
		if address.DADFailed {
			next.LastError = "SLAAC duplicate-address detection failed; choose a different interface_id"
			break
		}
	}
	s.learned = next
	return nil
}

func promoteDNS[T any](items []T, fresh []string, key func(T) string) {
	order := make(map[string]int, len(fresh))
	for i, value := range fresh {
		order[value] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, aNew := order[key(items[i])]
		b, bNew := order[key(items[j])]
		if aNew != bNew {
			return aNew
		}
		return aNew && a < b
	})
}

func (s *state) dadFailed(address string) bool {
	for i := range s.learned.Addresses {
		if s.learned.Addresses[i].Address == address && !s.learned.Addresses[i].DADFailed {
			s.learned.Addresses[i].DADFailed = true
			s.learned.LastError = "SLAAC duplicate-address detection failed; choose a different interface_id"
			return true
		}
	}
	return false
}
