// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

type engine struct {
	config      Config
	revision    uint64
	writtenAt   time.Time
	now         func() time.Time
	leases      map[string]storedLease
	quarantines map[string]quarantine
	offers      map[string]offer
	stats       Stats
	persist     func(journal) error
	probe       func(context.Context, netip.Addr, net.HardwareAddr) (bool, error)
	events      []Event
	mtu         int
}

func newEngine(c Config, j journal, now func() time.Time, persist func(journal) error) *engine {
	e := &engine{config: c, now: now, offers: map[string]offer{}, persist: persist, mtu: 1500}
	e.restore(j)
	return e
}
func (e *engine) restore(j journal) {
	e.revision, e.writtenAt = j.Revision, j.WrittenAt
	e.leases = make(map[string]storedLease, len(j.Leases))
	e.quarantines = make(map[string]quarantine, len(j.Quarantines))
	for _, l := range j.Leases {
		e.leases[leaseKey(l.Lease)] = l
	}
	for _, q := range j.Quarantines {
		e.quarantines[q.IPAddress] = q
	}
}
func identity(m message) string {
	if b := m.Options[61]; len(b) > 0 {
		return "id:" + hex.EncodeToString(b)
	}
	return "mac:" + m.MAC.String()
}
func leaseKey(l Lease) string {
	if l.ClientID != "" {
		return l.Scope + "|id:" + l.ClientID
	}
	return l.Scope + "|mac:" + l.MACAddress
}
func (e *engine) scopeFor(m message) (Scope, *Reservation, bool) {
	s := dhcpserverconfig.SelectScope(e.config, m.MAC)
	var reservation *Reservation
	for i := range e.config.Reservations {
		r := &e.config.Reservations[i]
		if r.MACAddress == m.MAC.String() {
			reservation = r
			if r.Scope != "" {
				for _, candidate := range append([]Scope{e.config.Primary}, e.config.Scopes...) {
					if candidate.Name == r.Scope {
						s = candidate
						break
					}
				}
			}
			break
		}
	}
	if !m.GIAddr.IsUnspecified() {
		p, _ := netip.ParsePrefix(s.Prefix)
		if !p.Contains(m.GIAddr) {
			return Scope{}, nil, false
		}
	}
	return s, reservation, true
}
func (e *engine) trusted(p packet, m message) bool {
	server, _ := netip.ParseAddr(e.config.ServerAddress)
	if p.Destination != server && p.Destination != broadcast4 {
		return false
	}
	if m.GIAddr.IsUnspecified() {
		if p.SourcePort != 68 || m.Hops != 0 || len(m.Options[82]) > 0 {
			return false
		}
		if m.CIAddr.IsUnspecified() {
			return p.Source.IsUnspecified()
		}
		return p.Source == m.CIAddr && !p.Source.IsMulticast() && !p.Source.IsLoopback()
	}
	if p.SourcePort != 67 || p.Destination != server {
		return false
	}
	src, gi := false, false
	for _, v := range e.config.TrustedRelays {
		a, _ := netip.ParseAddr(v)
		src = src || a == p.Source
		gi = gi || a == m.GIAddr
	}
	if !src || !gi {
		return false
	}
	if b, ok := m.Options[82]; ok {
		// RFC 3046 relay suboptions are opaque, but their lengths must be
		// self-consistent before the server echoes them to the trusted relay.
		if len(b) == 0 || len(b) > 255 {
			return false
		}
		for i := 0; i < len(b); {
			if i+2 > len(b) || i+2+int(b[i+1]) > len(b) {
				return false
			}
			i += 2 + int(b[i+1])
		}
	}
	return true
}
func (e *engine) handle(ctx context.Context, p packet) (*response, error) {
	e.stats.Received++
	m, err := decodeRequest(p.Data)
	if err != nil || !e.trusted(p, m) {
		e.stats.Rejected++
		return nil, nil
	}
	// Some clients retain an old ciaddr during discovery or an option-50
	// request. Verify the actual sender first, then use the requested-address
	// exchange, as dnsmasq does, without treating that old address as renewal.
	if m.Type == msgDiscover || len(m.Options[50]) != 0 {
		m.CIAddr = zero4
	}
	s, r, ok := e.scopeFor(m)
	if !ok {
		e.stats.Rejected++
		return nil, nil
	}
	now := e.now()
	key := s.Name + "|" + identity(m)
	server, _ := netip.ParseAddr(e.config.ServerAddress)
	selected := optionAddress(m, 54)
	switch m.Type {
	case msgDiscover:
		if !selected.IsUnspecified() {
			e.stats.Rejected++
			return nil, nil
		}
		address, ok := e.chooseAddress(s, r, m, key, now)
		if !ok {
			e.stats.Exhausted++
			return nil, nil
		}
		if !e.alreadyBound(address, key, now) && m.GIAddr.IsUnspecified() {
			conflict, err := e.probe(ctx, address, m.MAC)
			if err != nil {
				return nil, err
			}
			if conflict {
				if err := e.markConflict(address, now); err != nil {
					return nil, err
				}
				return nil, nil
			}
		}
		if _, ok := e.offers[key]; !ok && len(e.offers) >= e.config.MaxLeases {
			e.stats.Exhausted++
			return nil, nil
		}
		seconds := leaseDuration(s, r, m)
		if old, ok := e.leases[key]; ok && old.IPAddress == address.String() && old.ExpiresAt.After(now) {
			seconds = uint32(max(1, int(old.ExpiresAt.Sub(now)/time.Second)))
		}
		l := e.newLease(s, r, m, address, now, seconds)
		e.offers[key] = offer{Lease: l, XID: m.XID, Until: now.Add(time.Duration(e.config.OfferSeconds) * time.Second), Probed: m.GIAddr.IsUnspecified()}
		reply, err := e.reply(m, msgOffer, s, r, address, seconds)
		if err == nil && reply != nil {
			e.stats.Offered++
		}
		return reply, err
	case msgRequest:
		_, hasRequested := m.Options[50]
		var address netip.Addr
		selecting := !selected.IsUnspecified()
		if selecting {
			if !hasRequested {
				e.stats.Rejected++
				return nil, nil
			}
			if selected != server {
				delete(e.offers, key)
				return nil, nil
			}
			address = optionAddress(m, 50)
			offered, ok := e.offers[key]
			known := false
			if old, ok := e.leases[key]; ok {
				known = old.IPAddress == address.String() && old.HeldUntil.After(now)
			}
			if (!ok || !offered.Until.After(now) || offered.Lease.IPAddress != address.String() || offered.XID != m.XID) && !known {
				return e.nak(m, s, r)
			}
		} else if m.CIAddr.IsUnspecified() {
			if !hasRequested {
				e.stats.Rejected++
				return nil, nil
			}
			address = optionAddress(m, 50)
			// INIT-REBOOT on the correct subnet with no known binding must
			// remain silent, even in authoritative mode (RFC 2131 4.3.2).
			old, known := e.leases[key]
			staticKnown := r != nil && r.IPAddress == address.String()
			prefix, _ := netip.ParsePrefix(s.Prefix)
			if !known && !staticKnown && prefix.Contains(address) {
				return nil, nil
			}
			if known && old.IPAddress != address.String() {
				if e.config.Authoritative {
					return e.nak(m, s, r)
				}
				return nil, nil
			}
		} else {
			address = m.CIAddr
			old, known := e.leases[key]
			staticKnown := r != nil && r.IPAddress == address.String()
			if !known && !staticKnown {
				if e.config.Authoritative && !e.legal(s, r, address) {
					return e.nak(m, s, r)
				}
				return nil, nil
			}
			if known && old.IPAddress != address.String() {
				if e.config.Authoritative {
					return e.nak(m, s, r)
				}
				return nil, nil
			}
		}
		if !e.legal(s, r, address) || e.inUse(address, key, now) {
			if selecting || e.config.Authoritative {
				return e.nak(m, s, r)
			}
			return nil, nil
		}
		if old, ok := e.leases[key]; ok && old.ExpiresAt.After(now) && old.IPAddress != address.String() {
			return e.nak(m, s, r)
		}
		offered, hasOffer := e.offers[key]
		recentProbe := hasOffer && offered.Probed && offered.Until.After(now) && offered.Lease.IPAddress == address.String()
		if !e.alreadyBound(address, key, now) && !recentProbe && m.GIAddr.IsUnspecified() {
			conflict, err := e.probe(ctx, address, m.MAC)
			if err != nil {
				return nil, err
			}
			if conflict {
				if err := e.markConflict(address, now); err != nil {
					return nil, err
				}
				delete(e.offers, key)
				if selecting || e.config.Authoritative {
					return e.nak(m, s, r)
				}
				return nil, nil
			}
		}
		if _, ok := e.leases[key]; !ok && len(e.leases) >= e.config.MaxLeases {
			e.stats.Exhausted++
			return nil, nil
		}
		seconds := leaseDuration(s, r, m)
		l := e.newLease(s, r, m, address, now, seconds)
		kind := "bound"
		if old, ok := e.leases[key]; ok && old.IPAddress == l.IPAddress {
			l.AcquiredAt = old.AcquiredAt
			if old.ExpiresAt.After(now) {
				kind = "renewed"
			}
		}
		e.leases[key] = storedLease{Lease: l, HeldUntil: l.ExpiresAt.Add(time.Duration(e.config.StickyHoldDays) * 24 * time.Hour)}
		delete(e.offers, key)
		if err := e.save(now, Event{Type: kind, Lease: &l}); err != nil {
			return nil, err
		}
		reply, err := e.reply(m, msgACK, s, r, address, seconds)
		if err == nil && reply != nil {
			e.stats.Acknowledged++
		}
		return reply, err
	case msgDecline:
		if selected != server || !m.CIAddr.IsUnspecified() || len(m.Options[50]) != 4 {
			e.stats.Rejected++
			return nil, nil
		}
		address := optionAddress(m, 50)
		old, known := e.leases[key]
		o, offered := e.offers[key]
		if !(known && old.IPAddress == address.String() && old.ExpiresAt.After(now)) && !(offered && o.Lease.IPAddress == address.String() && o.Until.After(now)) {
			e.stats.Rejected++
			return nil, nil
		}
		if len(e.quarantines) >= e.config.MaxLeases {
			e.stats.Exhausted++
			return nil, nil
		}
		var lease Lease
		if known && old.IPAddress == address.String() {
			lease = old.Lease
			delete(e.leases, key)
		} else {
			lease = o.Lease
		}
		delete(e.offers, key)
		e.quarantines[address.String()] = quarantine{IPAddress: address.String(), Until: now.Add(time.Duration(e.config.DeclineSeconds) * time.Second)}
		e.stats.Declined++
		return nil, e.save(now, Event{Type: "declined", Lease: &lease})
	case msgRelease:
		if selected != server || m.CIAddr.IsUnspecified() || len(m.Options[50]) != 0 {
			e.stats.Rejected++
			return nil, nil
		}
		old, known := e.leases[key]
		if !known || old.IPAddress != m.CIAddr.String() || !old.ExpiresAt.After(now) {
			return nil, nil
		}
		lease := old.Lease
		old.ExpiresAt = now
		old.HeldUntil = now.Add(time.Duration(e.config.StickyHoldDays) * 24 * time.Hour)
		old.Expired = true
		if old.HeldUntil.After(now) {
			e.leases[key] = old
		} else {
			delete(e.leases, key)
		}
		delete(e.offers, key)
		e.stats.Released++
		return nil, e.save(now, Event{Type: "released", Lease: &lease})
	case msgInform:
		if m.CIAddr.IsUnspecified() || !selected.IsUnspecified() || len(m.Options[50]) != 0 {
			e.stats.Rejected++
			return nil, nil
		}
		prefix, _ := netip.ParsePrefix(s.Prefix)
		if !dhcpserverconfig.Usable(prefix, m.CIAddr) {
			e.stats.Rejected++
			return nil, nil
		}
		return e.reply(m, msgACK, s, r, zero4, 0)
	}
	return nil, nil
}

func (e *engine) reply(m message, typ byte, s Scope, r *Reservation, address netip.Addr, seconds uint32) (*response, error) {
	options, err := replyOptions(e.config, s, r, m, typ, seconds)
	if err != nil {
		return nil, err
	}
	server, _ := netip.ParseAddr(e.config.ServerAddress)
	data, err := encodeReply(m, typ, address, server, options, e.mtu)
	if err != nil {
		// A client's constrained maximum size plus an oversized echoed
		// identifier/relay option must not terminate other clients' service.
		e.stats.Rejected++
		return nil, nil
	}
	response := makeResponse(m, typ, data, address)
	return &response, nil
}
func (e *engine) nak(m message, s Scope, r *Reservation) (*response, error) {
	reply, err := e.reply(m, msgNAK, s, r, zero4, 0)
	if err == nil && reply != nil {
		e.stats.NAK++
	}
	return reply, err
}
func (e *engine) newLease(s Scope, r *Reservation, m message, address netip.Addr, now time.Time, seconds uint32) Lease {
	hostname := strings.TrimSuffix(string(m.Options[12]), ".")
	if !dhcpserverconfig.ValidHostname(hostname) {
		hostname = ""
	}
	if r != nil && r.Hostname != "" {
		hostname = r.Hostname
	}
	return Lease{ClientID: hex.EncodeToString(m.Options[61]), MACAddress: m.MAC.String(), IPAddress: address.String(), Hostname: hostname, Scope: s.Name, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(time.Duration(seconds) * time.Second), Reserved: r != nil}
}
func leaseDuration(s Scope, r *Reservation, m message) uint32 {
	seconds := s.LeaseSeconds
	if r != nil && r.LeaseSeconds != 0 {
		seconds = r.LeaseSeconds
	}
	if requested := m.Options[51]; len(requested) == 4 {
		n := binary.BigEndian.Uint32(requested)
		if n >= 60 && uint64(n) < uint64(seconds) {
			seconds = int(n)
		}
	}
	return uint32(seconds)
}
func (e *engine) legal(s Scope, r *Reservation, a netip.Addr) bool {
	p, _ := netip.ParsePrefix(s.Prefix)
	if !dhcpserverconfig.Usable(p, a) || a.String() == e.config.ServerAddress {
		return false
	}
	if dhcpserverconfig.InfrastructureAddress(e.config, a.String()) {
		return false
	}
	if r != nil {
		return r.IPAddress == a.String()
	}
	for _, reserved := range e.config.Reservations {
		if reserved.IPAddress == a.String() {
			return false
		}
	}
	start, _ := netip.ParseAddr(s.Start)
	end, _ := netip.ParseAddr(s.End)
	return a.Compare(start) >= 0 && a.Compare(end) <= 0
}
func (e *engine) inUse(a netip.Addr, key string, now time.Time) bool {
	if q, ok := e.quarantines[a.String()]; ok && q.Until.After(now) {
		return true
	}
	for k, l := range e.leases {
		if k != key && l.IPAddress == a.String() && l.HeldUntil.After(now) {
			return true
		}
	}
	for k, o := range e.offers {
		if k != key && o.Lease.IPAddress == a.String() && o.Until.After(now) {
			return true
		}
	}
	return false
}
func (e *engine) alreadyBound(a netip.Addr, key string, now time.Time) bool {
	l, ok := e.leases[key]
	return ok && l.IPAddress == a.String() && l.ExpiresAt.After(now)
}
func (e *engine) chooseAddress(s Scope, r *Reservation, m message, key string, now time.Time) (netip.Addr, bool) {
	valid := func(a netip.Addr) bool { return a.IsValid() && e.legal(s, r, a) && !e.inUse(a, key, now) }
	if old, ok := e.leases[key]; ok && old.HeldUntil.After(now) {
		a, _ := netip.ParseAddr(old.IPAddress)
		if valid(a) {
			return a, true
		}
		if old.ExpiresAt.After(now) {
			return netip.Addr{}, false
		}
	}
	if o, ok := e.offers[key]; ok && o.Until.After(now) {
		a, _ := netip.ParseAddr(o.Lease.IPAddress)
		if valid(a) {
			return a, true
		}
	}
	if r != nil {
		a, _ := netip.ParseAddr(r.IPAddress)
		return a, valid(a)
	}
	if a := optionAddress(m, 50); valid(a) {
		return a, true
	}
	start, _ := netip.ParseAddr(s.Start)
	end, _ := netip.ParseAddr(s.End)
	first, last := ipNumber(start), ipNumber(end)
	span := uint64(last) - uint64(first) + 1
	h := sha256.Sum256([]byte(key))
	offset := uint64(binary.BigEndian.Uint32(h[:4])) % span
	// At most the bounded set of occupied addresses plus one candidate need
	// inspection, even when a cloud pool covers a very large prefix.
	// Option 3 may reserve many routers, including per-reservation overrides.
	// Count these exclusions too; counting duplicates is harmless.
	optionRouters := func(options []dhcpserverconfig.Option) int {
		if len(options) == 0 {
			return 0
		}
		encoded, _ := dhcpserverconfig.EncodeOptions(options) // validated configuration
		return len(encoded[3]) / 4
	}
	excludedRouters := optionRouters(e.config.Primary.Options)
	for _, scope := range e.config.Scopes {
		excludedRouters += optionRouters(scope.Options)
	}
	for _, reservation := range e.config.Reservations {
		excludedRouters += optionRouters(reservation.Options)
	}
	bound := min(span, uint64(len(e.leases)+len(e.offers)+len(e.quarantines)+len(e.config.Reservations)+len(e.config.Scopes)+excludedRouters+3))
	for n := uint64(0); n < bound; n++ {
		a := numberIP(first + uint32((offset+n)%span))
		if valid(a) {
			return a, true
		}
	}
	return netip.Addr{}, false
}
func ipNumber(a netip.Addr) uint32 { v := a.As4(); return binary.BigEndian.Uint32(v[:]) }
func numberIP(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
func (e *engine) markConflict(a netip.Addr, now time.Time) error {
	if len(e.quarantines) >= e.config.MaxLeases {
		e.stats.Exhausted++
		return nil
	}
	e.stats.Conflicts++
	for key, l := range e.leases {
		if l.IPAddress == a.String() {
			delete(e.leases, key)
		}
	}
	for key, o := range e.offers {
		if o.Lease.IPAddress == a.String() {
			delete(e.offers, key)
		}
	}
	e.quarantines[a.String()] = quarantine{IPAddress: a.String(), Until: now.Add(time.Duration(e.config.DeclineSeconds) * time.Second)}
	return e.save(now, Event{Type: "conflict", Lease: &Lease{IPAddress: a.String()}})
}
func (e *engine) journal() journal {
	j := journal{Version: journalVersion, Resource: e.config.Resource, Interface: e.config.Interface, Revision: e.revision, WrittenAt: e.writtenAt}
	for _, l := range e.leases {
		j.Leases = append(j.Leases, l)
	}
	for _, q := range e.quarantines {
		j.Quarantines = append(j.Quarantines, q)
	}
	return j
}
func (e *engine) save(now time.Time, events ...Event) error {
	j := e.journal()
	j.Revision++
	j.WrittenAt = now
	if err := e.persist(j); err != nil {
		return fmt.Errorf("persist DHCP binding before reply: %w", err)
	}
	e.revision, e.writtenAt = j.Revision, j.WrittenAt
	for _, event := range events {
		event.Resource = e.config.Resource
		event.At = now
		e.events = append(e.events, event)
	}
	return nil
}
func (e *engine) sweep(now time.Time) error {
	changed := false
	var events []Event
	for k, l := range e.leases {
		if !l.ExpiresAt.After(now) && !l.Expired {
			l.Expired = true
			e.leases[k] = l
			changed = true
			e.stats.Expired++
			copy := l.Lease
			events = append(events, Event{Type: "expired", Lease: &copy})
		}
		if !l.HeldUntil.After(now) {
			delete(e.leases, k)
			changed = true
		}
	}
	for ip, q := range e.quarantines {
		if !q.Until.After(now) {
			delete(e.quarantines, ip)
			changed = true
		}
	}
	for k, o := range e.offers {
		if !o.Until.After(now) {
			delete(e.offers, k)
		}
	}
	if changed {
		return e.save(now, events...)
	}
	return nil
}
func (e *engine) snapshot(epoch string, sequence uint64, clock boottime.Clock, active bool) Snapshot {
	now := clock.WallTime
	s := Snapshot{Resource: e.config.Resource, Interface: e.config.Interface, ServerAddress: e.config.ServerAddress, Epoch: epoch, Sequence: sequence, Active: active, ObservedAt: now, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(dhcpserverconfig.ObservationLifetime), Leases: []Lease{}, Stats: e.stats}
	s.Stats.Offers = len(e.offers)
	s.Standby, s.CheckpointRevision = e.config.Standby, e.revision
	s.Stats.Quarantined = len(e.quarantines)
	for _, l := range e.leases {
		if active && dhcpserverconfig.ValidateLease(e.config, l.Lease, now) == nil {
			s.Leases = append(s.Leases, l.Lease)
		}
	}
	sort.Slice(s.Leases, func(i, j int) bool { return leaseKey(s.Leases[i]) < leaseKey(s.Leases[j]) })
	return s
}
