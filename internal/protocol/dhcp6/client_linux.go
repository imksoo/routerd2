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

	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/vishvananda/netlink"
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
			if now.Before(a.ValidUntil) {
				addresses = append(addresses, assignment{prefix: netip.PrefixFrom(netip.MustParseAddr(a.Address), 128)})
			}
		}
		for _, p := range c.snapshot.Prefixes {
			if now.Before(p.ValidUntil) {
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
func deadline(now time.Time, seconds uint32) time.Time {
	if seconds == ^uint32(0) {
		return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
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
func (c *client) accept(m message, now time.Time) error {
	if !bytes.Equal(m.client, c.duid) || len(m.server) < 4 || len(m.server) > 128 {
		return nil
	}
	if c.info.kind != 0 && m.xid == c.info.xid && m.kind == reply {
		if m.status == 0 {
			c.information(m, now)
			c.info = exchange{next: c.snapshot.InformationRefreshAt}
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
	if m.status == 3 && (e.kind == renew || e.kind == rebind) {
		c.server = slices.Clone(m.server)
		return c.begin(request, now)
	}
	if m.status != 0 {
		return nil
	}
	if e.kind == decline {
		return c.begin(solicit, now)
	}
	if m.na != nil && m.na.iaid != c.cfg.IAID || m.pd != nil && m.pd.iaid != c.cfg.IAID {
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
	var renewAt, rebindAt time.Time
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
			renewAt = earlier(renewAt, deadline(now, t1))
			rebindAt = earlier(rebindAt, deadline(now, t2))
		}
	}
	if m.na != nil && m.na.status == 0 {
		addresses := slices.Clone(next.Addresses)
		for _, a := range m.na.leases {
			value := a.prefix.Addr().String()
			addresses = slices.DeleteFunc(addresses, func(v dhcp6config.Address) bool { return v.Address == value })
			if a.valid > 0 {
				addresses = append(addresses, dhcp6config.Address{Address: value, PreferredUntil: deadline(now, a.preferred), ValidUntil: deadline(now, a.valid)})
			}
		}
		next.Addresses = addresses
	}
	if m.pd != nil && m.pd.status == 0 {
		prefixes := slices.Clone(next.Prefixes)
		if len(m.pd.leases) > 0 {
			for i := range prefixes {
				present := false
				for _, a := range m.pd.leases {
					present = present || a.prefix.String() == prefixes[i].Prefix
				}
				if !present {
					prefixes[i].PreferredUntil = earlier(prefixes[i].PreferredUntil, now)
				}
			}
		}
		for _, a := range m.pd.leases {
			value := a.prefix.String()
			prefixes = slices.DeleteFunc(prefixes, func(v dhcp6config.Prefix) bool { return v.Prefix == value })
			if a.valid > 0 {
				prefixes = append(prefixes, dhcp6config.Prefix{Prefix: value, PreferredUntil: deadline(now, a.preferred), ValidUntil: deadline(now, a.valid)})
			}
		}
		next.Prefixes = prefixes
	}
	if len(next.Addresses) > 64 || len(next.Prefixes) > 64 {
		return nil
	}
	c.server = slices.Clone(m.server)
	next.ServerDUID = hex.EncodeToString(m.server)
	c.snapshot = next
	c.snapshot.RenewAt = renewAt
	c.snapshot.RebindAt = rebindAt
	c.lease = exchange{}
	c.snapshot.State = "bound"
	if m.maxSolicit != 0 {
		c.maxSolicit = time.Duration(m.maxSolicit) * time.Second
	}
	if m.maxInformation != 0 {
		c.maxInformation = time.Duration(m.maxInformation) * time.Second
	}
	if len(m.dns)+len(m.domains)+len(m.sntp) > 0 || m.aftr != "" {
		c.information(m, now)
		c.info = exchange{next: c.snapshot.InformationRefreshAt}
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
func (c *client) information(m message, now time.Time) {
	c.snapshot.DNSServers = m.dns
	c.snapshot.DomainSearch = m.domains
	c.snapshot.SNTPServers = m.sntp
	c.snapshot.AFTRName = m.aftr
	c.snapshot.InfoUpdatedAt = now
	c.snapshot.InformationRefreshSeconds = m.refresh
	c.snapshot.InformationRefreshAt = time.Time{}
	if m.refresh != ^uint32(0) {
		c.snapshot.InformationRefreshAt = deadline(now, m.refresh)
	}
	if m.maxInformation != 0 {
		c.maxInformation = time.Duration(m.maxInformation) * time.Second
	}
}
func (c *client) advance(now time.Time) error {
	c.snapshot.Addresses = slices.DeleteFunc(slices.Clone(c.snapshot.Addresses), func(a dhcp6config.Address) bool { return !now.Before(a.ValidUntil) })
	c.snapshot.Prefixes = slices.DeleteFunc(slices.Clone(c.snapshot.Prefixes), func(p dhcp6config.Prefix) bool { return !now.Before(p.ValidUntil) })
	if c.cfg.RequestAddress || c.cfg.RequestPrefix {
		switch {
		case c.lease.kind == decline:
			if now.Sub(c.lease.started) >= 10*time.Second {
				return c.begin(solicit, now)
			}
		case !c.snapshot.LeaseUntil().IsZero() && !c.snapshot.RebindAt.IsZero() && !now.Before(c.snapshot.RebindAt) && (c.lease.kind == 0 || c.lease.kind == renew):
			return c.begin(rebind, now)
		case !c.snapshot.LeaseUntil().IsZero() && c.lease.kind == 0 && !c.snapshot.RenewAt.IsZero() && !now.Before(c.snapshot.RenewAt):
			return c.begin(renew, now)
		case c.snapshot.LeaseUntil().IsZero() && (c.lease.kind == 0 || c.lease.kind == renew || c.lease.kind == rebind):
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
	ifi, err := net.InterfaceByName(cfg.Interface)
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
	conn, err := listen(cfg.Interface, ifi.Index)
	if err != nil {
		return err
	}
	defer conn.Close()
	c := client{cfg: cfg, duid: duid, maxSolicit: 3600 * time.Second, maxInformation: 3600 * time.Second, snapshot: dhcp6config.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, IAID: cfg.IAID, ClientDUID: hex.EncodeToString(duid)}}
	now := time.Now()
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
		if reflect.DeepEqual(last, c.snapshot) {
			return nil
		}
		if err := effects.Update(ctx, c.snapshot); err != nil {
			return fmt.Errorf("publish DHCPv6 assignments: %w", err)
		}
		last = c.snapshot
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		withdrawn := dhcp6config.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, IAID: cfg.IAID, ClientDUID: hex.EncodeToString(duid), State: "stopped"}
		result = errors.Join(result, effects.Update(cleanup, withdrawn))
	}()
	if err := publish(); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now = time.Now()
		if err := c.advance(now); err != nil {
			return err
		}
		if cfg.RequestAddress && c.lease.kind != decline {
			failed, err := dadFailures(cfg.Interface, c.snapshot.Addresses)
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
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if from.Port != 547 {
			continue
		}
		m, err := decode(buf[:n])
		if err != nil {
			continue
		}
		if err := c.accept(m, time.Now()); err != nil {
			return err
		}
	}
}
func dadFailures(name string, addresses []dhcp6config.Address) ([]assignment, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	current, err := netlink.AddrList(l, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	var failed []assignment
	for _, a := range addresses {
		for _, live := range current {
			if live.Flags&unix.IFA_F_DADFAILED != 0 && live.IP.String() == a.Address {
				failed = append(failed, assignment{prefix: netip.PrefixFrom(netip.MustParseAddr(a.Address), 128)})
			}
		}
	}
	return failed, nil
}
func listen(name string, index int) (*net.UDPConn, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	addresses, err := netlink.AddrList(l, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	var source string
	for _, a := range addresses {
		if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			source = a.IP.String()
			break
		}
	}
	if source == "" {
		return nil, errors.New("DHCPv6 interface has no usable link-local address")
	}
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
	pc, err := lc.ListenPacket(context.Background(), "udp6", net.JoinHostPort(source+"%"+name, "546"))
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
