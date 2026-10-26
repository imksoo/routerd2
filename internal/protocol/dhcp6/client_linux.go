//go:build linux

// Package dhcp6 owns one DHCPv6 client exchange and UDP 546 socket per interface.
// Address installation and delegated LAN effects belong to the network writer.
package dhcp6

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/linklocal"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"golang.org/x/sys/unix"
)

type Effects interface {
	Update(context.Context, dhcp6config.Snapshot) error
}
type EffectsFunc func(context.Context, dhcp6config.Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s dhcp6config.Snapshot) error { return f(ctx, s) }

type exchange struct {
	kind          byte
	xid           uint32
	started, next time.Time
	timeout       time.Duration
	attempts      int
	offer         *message
}
type client struct {
	cfg                        dhcp6config.Config
	duid, server               []byte
	snapshot                   dhcp6config.Snapshot
	lease, info                exchange
	declined                   []assignment
	maxSolicit, maxInformation time.Duration
}

func (e *exchange) begin(kind byte, now time.Time) error {
	var xid [3]byte
	if _, err := rand.Read(xid[:]); err != nil {
		return err
	}
	*e = exchange{kind: kind, xid: uint32(xid[0])<<16 | uint32(xid[1])<<8 | uint32(xid[2]), started: now, next: now}
	if kind == solicit || kind == information {
		e.next = now.Add(time.Duration(mathrand.Int64N(int64(time.Second))))
	}
	return nil
}
func (c *client) begin(kind byte, now time.Time) error {
	switch kind {
	case solicit:
		c.snapshot.State = "soliciting"
	case request:
		c.snapshot.State = "requesting"
	case renew:
		c.snapshot.State = "renewing"
	case rebind:
		c.snapshot.State = "rebinding"
	case decline:
		c.snapshot.State = "declining"
	}
	return c.lease.begin(kind, now)
}
func (c *client) packet(e *exchange, now time.Time) []byte {
	if e.attempts == 0 {
		e.started = now
	}
	b := []byte{e.kind, byte(e.xid >> 16), byte(e.xid >> 8), byte(e.xid)}
	b = appendOption(b, 1, c.duid)
	if e.kind == request || e.kind == renew || e.kind == decline {
		b = appendOption(b, 2, c.server)
	}
	elapsed := min(65535, max(0, int(now.Sub(e.started)/(10*time.Millisecond))))
	b = appendOption(b, 8, binary.BigEndian.AppendUint16(nil, uint16(elapsed)))
	if e.kind != decline {
		b = appendOption(b, 6, []byte{0, 23, 0, 24, 0, 31, 0, 64, 0, 32, 0, 82, 0, 83})
	}
	if e.kind == information {
		return b
	}
	var addresses, prefixes []assignment
	if e.kind == request && e.offer != nil {
		if e.offer.na != nil {
			addresses = e.offer.na.leases
		}
		if e.offer.pd != nil {
			prefixes = e.offer.pd.leases
		}
	} else {
		for _, a := range c.snapshot.Addresses {
			if a.ValidUntil.Live(boottime.Clock{BootID: c.snapshot.BootID, BoottimeNanoseconds: now.UnixNano()}) {
				addresses = append(addresses, assignment{prefix: netip.PrefixFrom(netip.MustParseAddr(a.Address), 128)})
			}
		}
		for _, p := range c.snapshot.Prefixes {
			if p.ValidUntil.Live(boottime.Clock{BootID: c.snapshot.BootID, BoottimeNanoseconds: now.UnixNano()}) {
				prefixes = append(prefixes, assignment{prefix: netip.MustParsePrefix(p.Prefix)})
			}
		}
	}
	if e.kind == decline {
		return appendOption(b, 3, encodeIA(3, c.cfg.IAID, c.declined))
	}
	if c.cfg.RequestAddress {
		b = appendOption(b, 3, encodeIA(3, c.cfg.IAID, addresses))
	}
	if c.cfg.RequestPrefix {
		if len(prefixes) == 0 {
			prefixes = []assignment{{prefix: netip.PrefixFrom(netip.IPv6Unspecified(), c.cfg.PrefixLength)}}
		}
		b = appendOption(b, 25, encodeIA(25, c.cfg.IAID, prefixes))
	}
	return b
}
func (c *client) transmit(conn *net.UDPConn, e *exchange, now time.Time) error {
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := conn.WriteToUDP(c.packet(e, now), &net.UDPAddr{IP: net.ParseIP("ff02::1:2"), Port: 547, Zone: c.cfg.Interface}); err != nil {
		return err
	}
	maximum := 120 * time.Second
	switch e.kind {
	case solicit:
		maximum = c.maxSolicit
	case information:
		maximum = c.maxInformation
	case request:
		maximum = 30 * time.Second
	case decline:
		maximum = time.Second
	}
	if e.timeout == 0 {
		e.timeout = time.Second
	} else {
		e.timeout = min(e.timeout*2, maximum)
	}
	e.next = now.Add(e.timeout + time.Duration(mathrand.Int64N(int64(e.timeout/5))) - e.timeout/10)
	e.attempts++
	return nil
}
func timers(ia *association) (uint32, uint32) {
	basis := ^uint32(0)
	for _, a := range ia.leases {
		if a.valid > 0 {
			basis = min(basis, a.preferred)
		}
	}
	t1, t2 := ia.t1, ia.t2
	if t1 == 0 {
		t1 = max(1, basis/2)
	}
	if t2 == 0 {
		t2 = max(t1, uint32(uint64(basis)*4/5))
	}
	return t1, t2
}
func (c *client) accept(m message, clock boottime.Clock) error {
	now := time.Unix(0, clock.BoottimeNanoseconds)
	if !bytes.Equal(m.client, c.duid) || len(m.server) < 4 || len(m.server) > 128 {
		return nil
	}
	if c.info.kind != 0 && m.xid == c.info.xid && m.kind == reply {
		if m.status == 0 {
			c.information(m, clock)
		}
		return nil
	}
	e := &c.lease
	if e.kind == 0 || m.xid != e.xid || (e.kind == solicit && m.kind != advertise) || (e.kind != solicit && m.kind != reply) {
		return nil
	}
	if e.kind == request || e.kind == renew || e.kind == decline {
		if !bytes.Equal(m.server, c.server) {
			return nil
		}
	}
	// A valid Decline Reply completes the exchange regardless of status,
	// including NoBinding after a lost acknowledgement (RFC 8415 18.2.10.2).
	if e.kind == decline {
		return c.begin(solicit, now)
	}
	if m.status == 3 && (e.kind == renew || e.kind == rebind) {
		c.server = slices.Clone(m.server)
		return c.begin(request, now)
	}
	if m.status != 0 {
		return nil
	}
	if !c.cfg.RequestAddress {
		m.na = nil
	}
	if !c.cfg.RequestPrefix {
		m.pd = nil
	}
	usableIA := false
	for _, ia := range []*association{m.na, m.pd} {
		if ia != nil && ia.status == 0 {
			for _, a := range ia.leases {
				if a.valid > 0 {
					usableIA = true
				}
			}
		}
	}
	if e.kind == solicit {
		if !usableIA {
			return nil
		}
		if e.offer == nil || m.preference > e.offer.preference {
			e.offer = &m
		}
		if m.preference == 255 {
			return c.selectOffer(now)
		}
		return nil
	}
	// A withdrawal is meaningful even when no lease survives the Reply.
	if m.na == nil && m.pd == nil {
		return nil
	}
	next := c.snapshot
	var renewAt, rebindAt lifetime.Deadline
	for _, ia := range []*association{m.na, m.pd} {
		if ia == nil {
			continue
		}
		if ia.status == 3 && (e.kind == renew || e.kind == rebind) {
			c.server = slices.Clone(m.server)
			return c.begin(request, now)
		}
		if ia.status != 0 {
			continue
		}
		t1, t2 := timers(ia)
		if len(ia.leases) > 0 {
			renewAt = lifetime.Minimum(clock, renewAt, lifetime.After(clock, t1))
			rebindAt = lifetime.Minimum(clock, rebindAt, lifetime.After(clock, t2))
		}
	}
	if m.na != nil && m.na.status == 0 {
		addresses := slices.Clone(next.Addresses)
		for _, a := range m.na.leases {
			value := a.prefix.Addr().String()
			addresses = slices.DeleteFunc(addresses, func(v dhcp6config.Address) bool { return v.Address == value })
			if a.valid > 0 {
				addresses = append(addresses, dhcp6config.Address{Address: value, PreferredUntil: lifetime.After(clock, a.preferred), ValidUntil: lifetime.After(clock, a.valid)})
			}
		}
		next.Addresses = addresses
	}
	if m.pd != nil && m.pd.status == 0 {
		prefixes := slices.Clone(next.Prefixes)
		for _, a := range m.pd.leases {
			value := a.prefix.String()
			prefixes = slices.DeleteFunc(prefixes, func(v dhcp6config.Prefix) bool { return v.Prefix == value })
			if a.valid > 0 {
				prefixes = append(prefixes, dhcp6config.Prefix{Prefix: value, PreferredUntil: lifetime.After(clock, a.preferred), ValidUntil: lifetime.After(clock, a.valid)})
			}
		}
		next.Prefixes = prefixes
	}
	if len(next.Addresses) > 64 || len(next.Prefixes) > 64 {
		return nil
	}
	c.server = slices.Clone(m.server)
	next.ServerDUID = hex.EncodeToString(m.server)
	// A partial Renew/Rebind Reply updates its own bindings but does not
	// acknowledge missing IAs or postpone their original retry/T2 budget
	// (RFC 8415 section 18.2.10.1).
	partial := (e.kind == renew || e.kind == rebind) && (c.cfg.RequestAddress && (m.na == nil || m.na.status != 0) || c.cfg.RequestPrefix && (m.pd == nil || m.pd.status != 0))
	if partial {
		renewAt = lifetime.Minimum(clock, next.RenewAt, renewAt)
		rebindAt = lifetime.Minimum(clock, next.RebindAt, rebindAt)
	} else {
		c.lease = exchange{}
		next.State = "bound"
	}
	next.RenewAt, next.RebindAt = renewAt, rebindAt
	c.snapshot = next
	if m.maxSolicit != 0 {
		c.maxSolicit = time.Duration(m.maxSolicit) * time.Second
	}
	if m.maxInformation != 0 {
		c.maxInformation = time.Duration(m.maxInformation) * time.Second
	}
	if len(m.dns)+len(m.domains)+len(m.sntp) > 0 || m.aftr != "" {
		c.information(m, clock)
		return nil
	}
	// Information has its own transaction: it cannot replace or postpone renew.
	if c.info.kind == 0 {
		return c.info.begin(information, now)
	}
	return nil
}
func (c *client) selectOffer(now time.Time) error {
	offer := c.lease.offer
	c.server = slices.Clone(offer.server)
	if err := c.begin(request, now); err != nil {
		return err
	}
	c.lease.offer = offer
	return nil
}
func (c *client) information(m message, clock boottime.Clock) {
	c.snapshot.DNSServers = m.dns
	c.snapshot.DomainSearch = m.domains
	c.snapshot.SNTPServers = m.sntp
	c.snapshot.AFTRName = m.aftr
	c.snapshot.InfoReceivedNanos = uint64(clock.BoottimeNanoseconds)
	c.snapshot.InformationRefreshSeconds = m.refresh
	c.snapshot.InformationRefreshAt = lifetime.After(clock, m.refresh)
	c.info = exchange{}
	if c.snapshot.InformationRefreshAt.Nanos != 0 {
		c.info.next = time.Unix(0, int64(c.snapshot.InformationRefreshAt.Nanos))
	}
	if m.maxInformation != 0 {
		c.maxInformation = time.Duration(m.maxInformation) * time.Second
	}
}
func (c *client) advance(clock boottime.Clock) error {
	now := time.Unix(0, clock.BoottimeNanoseconds)
	expiredAddress := func(a dhcp6config.Address) bool { return !a.ValidUntil.Live(clock) }
	if slices.ContainsFunc(c.snapshot.Addresses, expiredAddress) {
		c.snapshot.Addresses = slices.DeleteFunc(slices.Clone(c.snapshot.Addresses), expiredAddress)
	}
	expiredPrefix := func(p dhcp6config.Prefix) bool { return !p.ValidUntil.Live(clock) }
	if slices.ContainsFunc(c.snapshot.Prefixes, expiredPrefix) {
		c.snapshot.Prefixes = slices.DeleteFunc(slices.Clone(c.snapshot.Prefixes), expiredPrefix)
	}
	_, hasLease := c.snapshot.LeaseUntil()
	if c.cfg.RequestAddress || c.cfg.RequestPrefix {
		switch {
		case c.lease.kind == decline:
			if now.Sub(c.lease.started) >= 10*time.Second {
				return c.begin(solicit, now)
			}
		case hasLease && c.snapshot.RebindAt.BootID != "" && !c.snapshot.RebindAt.Live(clock) && (c.lease.kind == 0 || c.lease.kind == renew):
			return c.begin(rebind, now)
		case hasLease && c.lease.kind == 0 && c.snapshot.RenewAt.BootID != "" && !c.snapshot.RenewAt.Live(clock):
			return c.begin(renew, now)
		case !hasLease && (c.lease.kind == 0 || c.lease.kind == renew || c.lease.kind == rebind):
			return c.begin(solicit, now)
		case c.lease.kind == solicit && c.lease.offer != nil && !now.Before(c.lease.next):
			return c.selectOffer(now)
		case c.lease.kind == request && c.lease.attempts >= 10 && !now.Before(c.lease.next):
			return c.begin(solicit, now)
		}
	}
	if c.info.kind == 0 && !c.info.next.IsZero() && !now.Before(c.info.next) {
		return c.info.begin(information, now)
	}
	return nil
}

// Only the durable identity is stored. Restart always obtains a fresh server
// Reply; a saved lease is neither proof of address ownership nor a new lease.
type identity struct {
	Resource   string `json:"resource"`
	ClientDUID string `json:"client_duid"`
}

func Run(ctx context.Context, cfg dhcp6config.Config, effects Effects, ready func()) (result error) {
	if err := dhcp6config.Validate(cfg); err != nil {
		return err
	}
	if cfg.PrefixLength == 0 {
		cfg.PrefixLength = 60
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("DHCPv6 identity is already owned: %w", err)
	}
	var saved identity
	f, err := os.Open(cfg.StatePath)
	if err == nil {
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		err = dec.Decode(&saved)
		err = errors.Join(err, f.Close())
		if err != nil {
			return fmt.Errorf("read DHCPv6 identity: %w", err)
		}
		if saved.Resource != cfg.Resource {
			return errors.New("DHCPv6 identity belongs to another resource")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ifi, source, err := linklocal.Wait(ctx, cfg.Interface, netip.Addr{})
	if err != nil {
		return err
	}
	duidText := cfg.ClientDUID
	if duidText == "" {
		duidText = saved.ClientDUID
	}
	var duid []byte
	if duidText != "" {
		duid, err = dhcp6config.ParseDUID(duidText)
		if err != nil {
			return err
		}
	} else {
		if len(ifi.HardwareAddr) != 6 {
			return errors.New("automatic DHCPv6 DUID-LL requires Ethernet; configure client_duid")
		}
		duid = append([]byte{0, 3, 0, 1}, ifi.HardwareAddr...)
	}
	if err := writeIdentity(cfg.StatePath, identity{Resource: cfg.Resource, ClientDUID: hex.EncodeToString(duid)}); err != nil {
		return err
	}
	conn, err := listen(ctx, cfg.Interface, ifi.Index, source)
	if err != nil {
		return err
	}
	defer conn.Close()
	dadFD := -1
	if cfg.RequestAddress {
		dadFD, err = unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
		if err != nil {
			return fmt.Errorf("open IPv6 DAD notifications: %w", err)
		}
		defer unix.Close(dadFD)
		if err := unix.Bind(dadFD, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: unix.RTMGRP_IPV6_IFADDR}); err != nil {
			return fmt.Errorf("subscribe IPv6 DAD notifications: %w", err)
		}
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	c := client{cfg: cfg, duid: duid, maxSolicit: 3600 * time.Second, maxInformation: 3600 * time.Second, snapshot: dhcp6config.Snapshot{BootID: clock.BootID, Resource: cfg.Resource, Interface: cfg.Interface, IAID: cfg.IAID, ClientDUID: hex.EncodeToString(duid)}}
	now := time.Unix(0, clock.BoottimeNanoseconds)
	if cfg.RequestAddress || cfg.RequestPrefix {
		err = c.begin(solicit, now)
	} else {
		c.snapshot.State = "information"
		err = c.info.begin(information, now)
	}
	if err != nil {
		return err
	}
	var last dhcp6config.Snapshot
	publish := func() error {
		previous, current := last, c.snapshot
		previous.ObservedNanos, current.ObservedNanos = 0, 0
		if reflect.DeepEqual(previous, current) {
			return nil
		}
		c.snapshot.ObservedNanos = uint64(clock.BoottimeNanoseconds)
		if err := effects.Update(ctx, c.snapshot); err != nil {
			return fmt.Errorf("publish DHCPv6 assignments: %w", err)
		}
		last = c.snapshot
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		clock, err := boottime.ReadForBoot(c.snapshot.BootID)
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		withdrawn := dhcp6config.Snapshot{ObservedNanos: uint64(clock.BoottimeNanoseconds), BootID: clock.BootID, Resource: cfg.Resource, Interface: cfg.Interface, IAID: cfg.IAID, ClientDUID: hex.EncodeToString(duid), State: "stopped"}
		result = errors.Join(result, effects.Update(cleanup, withdrawn))
	}()
	if err := publish(); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	buf := make([]byte, 65536)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock, err = boottime.ReadForBoot(c.snapshot.BootID)
		if err != nil {
			return err
		}
		now = time.Unix(0, clock.BoottimeNanoseconds)
		if err := c.advance(clock); err != nil {
			return err
		}
		if cfg.RequestAddress {
			failed, err := dadFailures(ctx, dadFD, ifi.Index, c.snapshot.Addresses, buf)
			if err != nil {
				return err
			}
			if len(failed) > 0 {
				c.declined = failed
				addresses := slices.Clone(c.snapshot.Addresses)
				c.snapshot.Addresses = slices.DeleteFunc(addresses, func(a dhcp6config.Address) bool {
					for _, bad := range failed {
						if bad.prefix.Addr().String() == a.Address {
							return true
						}
					}
					return false
				})
				if err := c.begin(decline, now); err != nil {
					return err
				}
			}
		}
		if err := publish(); err != nil {
			return err
		}
		for _, e := range []*exchange{&c.lease, &c.info} {
			if e.kind != 0 && !now.Before(e.next) {
				if err := c.transmit(conn, e, now); err != nil {
					return err
				}
			}
		}
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return err
		}
		// Access gateways can reply from a high source port. accept matches
		// the client, transaction and selected server on this bound interface.
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		m, err := decode(buf[:n], cfg.IAID)
		if err != nil {
			continue
		}
		clock, err = boottime.ReadForBoot(c.snapshot.BootID)
		if err != nil {
			return err
		}
		if err := c.accept(m, clock); err != nil {
			return err
		}
	}
}

// Finite addresses are deleted on DAD failure. An address-list poll misses
// that transition; RTM_DELADDR retains IFA_F_DADFAILED. Subscribe before any
// assignment is published and drain in the same owner as the DHCP exchange.
func dadFailures(ctx context.Context, fd, index int, addresses []dhcp6config.Address, buf []byte) ([]assignment, error) {
	var failed []assignment
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, _, flags, from, err := unix.Recvmsg(fd, buf, nil, unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EAGAIN) {
			return failed, nil
		}
		if err != nil {
			return nil, fmt.Errorf("receive IPv6 DAD notification: %w", err)
		}
		sender, ok := from.(*unix.SockaddrNetlink)
		if !ok || sender.Pid != 0 {
			continue
		}
		if flags&unix.MSG_TRUNC != 0 {
			return nil, errors.New("truncated IPv6 DAD notification")
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range messages {
			if m.Header.Type == unix.NLMSG_OVERRUN {
				return nil, unix.ENOBUFS
			}
			if m.Header.Type != unix.RTM_NEWADDR && m.Header.Type != unix.RTM_DELADDR {
				continue
			}
			if len(m.Data) < unix.SizeofIfAddrmsg {
				return nil, errors.New("short IPv6 address notification")
			}
			if m.Data[0] != unix.AF_INET6 || int(binary.NativeEndian.Uint32(m.Data[4:8])) != index || m.Data[2]&unix.IFA_F_DADFAILED == 0 {
				continue
			}
			attrs, err := syscall.ParseNetlinkRouteAttr(&m)
			if err != nil {
				return nil, err
			}
			for _, attr := range attrs {
				if attr.Attr.Type != unix.IFA_ADDRESS || len(attr.Value) != net.IPv6len {
					continue
				}
				address := net.IP(attr.Value).String()
				for _, a := range addresses {
					if a.Address == address && !slices.ContainsFunc(failed, func(v assignment) bool { return v.prefix.Addr().String() == address }) {
						failed = append(failed, assignment{prefix: netip.PrefixFrom(netip.MustParseAddr(address), 128)})
					}
				}
			}
		}
	}
}
func listen(ctx context.Context, name string, index int, source netip.Addr) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			for _, s := range []struct{ option, value int }{{unix.IPV6_MULTICAST_IF, index}, {unix.IPV6_MULTICAST_HOPS, 1}} {
				if inner = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, s.option, s.value); inner != nil {
					return
				}
			}
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		})
		return errors.Join(err, inner)
	}}
	pc, err := lc.ListenPacket(ctx, "udp6", net.JoinHostPort(source.String()+"%"+name, "546"))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
func writeIdentity(path string, s identity) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp6-identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
