// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

type linkInfo struct {
	index, mtu int
	hardware   net.HardwareAddr
	source     netip.Addr
}
type state struct {
	identity string
	config   Config
	link     linkInfo
	iid      [8]byte
	learned  Snapshot
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
	s := &state{config: c, link: link, iid: iid, learned: Snapshot{Resource: c.Resource, Interface: c.Interface, SourceAddress: link.source.String(), InterfaceID: suffixString(iid), Active: true}}
	c.StatePath = ""
	b, err := json.Marshal(struct {
		Config                Config
		InterfaceIndex        int
		Hardware, Source, IID string
	}{c, link.index, link.hardware.String(), link.source.String(), suffixString(iid)})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	s.identity = hex.EncodeToString(sum[:])
	return s, nil
}

func clone(s Snapshot) Snapshot {
	s.Addresses = slices.Clone(s.Addresses)
	s.Prefixes = slices.Clone(s.Prefixes)
	s.Routes = slices.Clone(s.Routes)
	s.DNSServers = slices.Clone(s.DNSServers)
	s.SearchDomains = slices.Clone(s.SearchDomains)
	return s
}

func (s *state) snapshot(clock boottime.Clock) Snapshot {
	out := clone(s.learned)
	out.BootID, out.ObservedNanos, out.UpdatedAt = clock.BootID, uint64(clock.BoottimeNanoseconds), clock.WallTime
	return out
}

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

func expireSnapshot(s *Snapshot, now boottime.Clock) bool {
	changed := false
	before := len(s.Addresses) + len(s.Prefixes) + len(s.Routes) + len(s.DNSServers) + len(s.SearchDomains)
	s.Addresses = slices.DeleteFunc(s.Addresses, func(a Address) bool { return !a.ValidUntil.Live(now) })
	s.Prefixes = slices.DeleteFunc(s.Prefixes, func(p Prefix) bool { return !p.ValidUntil.Live(now) })
	s.Routes = slices.DeleteFunc(s.Routes, func(r Route) bool { return !r.ValidUntil.Live(now) })
	s.DNSServers = slices.DeleteFunc(s.DNSServers, func(d DNSServer) bool { return !d.ValidUntil.Live(now) })
	s.SearchDomains = slices.DeleteFunc(s.SearchDomains, func(d SearchDomain) bool { return !d.ValidUntil.Live(now) })
	for i := range s.Addresses {
		a := &s.Addresses[i]
		deprecated := !a.PreferredUntil.Live(now)
		if a.Deprecated != deprecated {
			a.Deprecated = deprecated
			changed = true
		}
	}
	return changed || before != len(s.Addresses)+len(s.Prefixes)+len(s.Routes)+len(s.DNSServers)+len(s.SearchDomains)
}

func (s *state) expire(now boottime.Clock) bool { return expireSnapshot(&s.learned, now) }

// validLifetime implements RFC 4862 5.5.3(e). A whitelist does not authenticate
// an RA, so it never bypasses the two-hour safeguard. Preferred lifetime still
// changes immediately, including immediate deprecation on a zero-lifetime PIO.
func validLifetime(old lifetime.Deadline, now boottime.Clock, advertised uint32) lifetime.Deadline {
	if advertised == ^uint32(0) {
		return lifetime.After(now, advertised)
	}
	next := time.Duration(advertised) * time.Second
	remaining := old.Remaining(now)
	if next > 2*time.Hour || next > remaining {
		return lifetime.After(now, advertised)
	}
	if remaining <= 2*time.Hour {
		return old
	}
	return lifetime.After(now, 7200)
}

func (s *state) handle(p packet, now boottime.Clock) error {
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
	expireSnapshot(&next, now)
	next.LastReceivedAt, next.LastRouter, next.ReceivedNanos = now.WallTime.UTC(), a.router.String(), uint64(now.BoottimeNanoseconds)
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
	putRoute := func(prefix netip.Prefix, pref int, seconds uint32) {
		r := Route{Destination: prefix.String(), Gateway: a.router.String(), Preference: pref, ValidUntil: lifetime.After(now, seconds)}
		next.Routes = update(next.Routes, func(old Route) bool { return old.Destination == r.Destination && old.Gateway == r.Gateway }, r, seconds == 0)
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
			v := Prefix{Prefix: p.prefix.String(), ValidUntil: lifetime.After(now, p.valid)}
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
		v := Address{Address: address, Prefix: p.prefix.String(), ValidUntil: lifetime.After(now, p.valid), PreferredUntil: lifetime.After(now, p.preferred), Deprecated: p.preferred == 0}
		if old != nil {
			v.ValidUntil = validLifetime(old.ValidUntil, now, p.valid)
			v.DADFailed = old.DADFailed
		}
		next.Addresses = update(next.Addresses, func(old Address) bool { return old.Address == address }, v, false)
	}
	if enabled(s.config.DNS) {
		newServers, newDomains := []string{}, []string{}
		for _, d := range a.dns {
			for _, address := range d.addresses {
				v := DNSServer{Address: address.String(), Router: a.router.String(), ValidUntil: lifetime.After(now, d.lifetime)}
				if d.lifetime != 0 && !slices.ContainsFunc(next.DNSServers, func(old DNSServer) bool { return old.Address == v.Address }) && !slices.Contains(newServers, v.Address) {
					newServers = append(newServers, v.Address)
				}
				next.DNSServers = update(next.DNSServers, func(old DNSServer) bool { return old.Address == v.Address }, v, d.lifetime == 0)
			}
			for _, domain := range d.domains {
				v := SearchDomain{Domain: domain, Router: a.router.String(), ValidUntil: lifetime.After(now, d.lifetime)}
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
