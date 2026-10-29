//go:build linux

// Package ra implements native IPv6 router advertisements and solicitation
// responses. It owns no interface addresses, routes or forwarding sysctls.
package ra

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/linklocal"
	"golang.org/x/sys/unix"
)

type Config struct {
	ActiveUntil           lifetime.Deadline `json:"active_until,omitzero"`
	Authority             lifetime.Deadline `json:"authority,omitzero"`
	Resource              string            `json:"resource"`
	Interface             string            `json:"interface"`
	SourceAddress         string            `json:"source_address,omitempty"`
	Prefixes              []Prefix          `json:"prefixes,omitempty"`
	RDNSS                 []string          `json:"rdnss,omitempty"`
	DNSSL                 []string          `json:"dnssl,omitempty"`
	LearnedRDNSS          []DNSServer       `json:"learned_rdnss,omitempty"`
	LearnedDNSSL          []SearchDomain    `json:"learned_dnssl,omitempty"`
	MinIntervalSeconds    int               `json:"min_interval_seconds,omitempty"`
	MaxIntervalSeconds    int               `json:"max_interval_seconds,omitempty"`
	RouterLifetimeSeconds int               `json:"router_lifetime_seconds"`
	DNSSLifetimeSeconds   int               `json:"dns_lifetime_seconds"`
	MTU                   int               `json:"mtu,omitempty"`
	Managed               bool              `json:"managed,omitempty"`
	OtherConfig           bool              `json:"other_config,omitempty"`
	RouterPreference      string            `json:"router_preference,omitempty"`
}
type Prefix struct {
	Prefix                   string `json:"prefix"`
	ValidLifetimeSeconds     uint32 `json:"valid_lifetime_seconds"`
	PreferredLifetimeSeconds uint32 `json:"preferred_lifetime_seconds"`
	OnLink                   bool   `json:"on_link"`
	Autonomous               bool   `json:"autonomous"`
	// Absolute deadlines let the PD compiler retain old prefixes without
	// refreshing their lifetimes on every advertisement or worker restart.
	ValidUntil     lifetime.Deadline `json:"valid_until,omitempty"`
	PreferredUntil lifetime.Deadline `json:"preferred_until,omitempty"`
}
type Snapshot struct {
	Resource            string    `json:"resource"`
	Interface           string    `json:"interface"`
	Sent                uint64    `json:"sent"`
	LastSent            time.Time `json:"last_sent,omitempty"`
	LastSolicitation    time.Time `json:"last_solicitation,omitempty"`
	Withdrawing         bool      `json:"withdrawing"`
	Generation          uint64    `json:"generation"`
	LastConfigUpdate    time.Time `json:"last_config_update,omitempty"`
	RejectedUpdates     uint64    `json:"rejected_updates"`
	LastUpdateError     string    `json:"last_update_error,omitempty"`
	WithdrawingPrefixes []string  `json:"withdrawing_prefixes,omitempty"`
}
type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

func normalized(c Config) Config {
	c.Prefixes = append([]Prefix(nil), c.Prefixes...)
	c.RDNSS = append([]string(nil), c.RDNSS...)
	c.DNSSL = append([]string(nil), c.DNSSL...)
	c.LearnedRDNSS = append([]DNSServer(nil), c.LearnedRDNSS...)
	c.LearnedDNSSL = append([]SearchDomain(nil), c.LearnedDNSSL...)
	if source, err := netip.ParseAddr(c.SourceAddress); err == nil {
		c.SourceAddress = source.String()
	}
	for i := range c.Prefixes {
		if p, err := netip.ParsePrefix(c.Prefixes[i].Prefix); err == nil {
			c.Prefixes[i].Prefix = p.String()
		}
		c.Prefixes[i].ValidUntil.UTC = c.Prefixes[i].ValidUntil.UTC.UTC()
		c.Prefixes[i].PreferredUntil.UTC = c.Prefixes[i].PreferredUntil.UTC.UTC()
	}
	for i := range c.RDNSS {
		if a, err := netip.ParseAddr(c.RDNSS[i]); err == nil {
			c.RDNSS[i] = a.String()
		}
	}
	for i := range c.DNSSL {
		c.DNSSL[i] = strings.TrimSuffix(strings.ToLower(c.DNSSL[i]), ".")
	}
	for i := range c.LearnedRDNSS {
		if a, err := netip.ParseAddr(c.LearnedRDNSS[i].Address); err == nil {
			c.LearnedRDNSS[i].Address = a.String()
		}
		c.LearnedRDNSS[i].ValidUntil.UTC = c.LearnedRDNSS[i].ValidUntil.UTC.UTC()
	}
	for i := range c.LearnedDNSSL {
		c.LearnedDNSSL[i].Domain = strings.TrimSuffix(strings.ToLower(c.LearnedDNSSL[i].Domain), ".")
		c.LearnedDNSSL[i].ValidUntil.UTC = c.LearnedDNSSL[i].ValidUntil.UTC.UTC()
	}
	if c.MaxIntervalSeconds == 0 {
		c.MaxIntervalSeconds = 600
	}
	if c.MinIntervalSeconds == 0 {
		c.MinIntervalSeconds = c.MaxIntervalSeconds / 3
	}
	return c
}
func Validate(c Config) error {
	if err := c.ActiveUntil.Validate(); err != nil {
		return err
	}
	if err := c.Authority.Validate(); err != nil {
		return err
	}
	c = normalized(c)
	if strings.TrimSpace(c.Resource) == "" {
		return errors.New("ra resource is required")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") {
		return errors.New("ra interface is invalid")
	}
	if c.SourceAddress != "" {
		a, err := netip.ParseAddr(c.SourceAddress)
		if err != nil || !a.Is6() || !a.IsLinkLocalUnicast() || a.Zone() != "" {
			return errors.New("RA source_address must be an IPv6 link-local address without a zone")
		}
	}
	if c.MaxIntervalSeconds < 4 || c.MaxIntervalSeconds > 1800 || c.MinIntervalSeconds < 3 || 4*c.MinIntervalSeconds > 3*c.MaxIntervalSeconds {
		return errors.New("RA intervals require max 4..1800 seconds, min >=3 and min <= 0.75*max")
	}
	if c.RouterLifetimeSeconds < 0 || c.RouterLifetimeSeconds > 9000 || (c.RouterLifetimeSeconds != 0 && c.RouterLifetimeSeconds < c.MaxIntervalSeconds) {
		return errors.New("RA router lifetime must be zero or max_interval..9000 seconds")
	}
	if c.DNSSLifetimeSeconds < 0 || int64(c.DNSSLifetimeSeconds) > int64(^uint32(0)) {
		return errors.New("RA DNS lifetime must fit uint32 seconds")
	}
	if c.MTU != 0 && (c.MTU < 1280 || c.MTU > 65535) {
		return errors.New("RA MTU must be zero or 1280..65535")
	}
	switch c.RouterPreference {
	case "", "medium", "high", "low":
	default:
		return errors.New("RA router_preference must be medium, high or low")
	}
	seen := map[string]bool{}
	for _, v := range c.Prefixes {
		p, err := netip.ParsePrefix(v.Prefix)
		if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().IsMulticast() || p.Addr().IsLinkLocalUnicast() || p != p.Masked() {
			return fmt.Errorf("invalid RA prefix %q", v.Prefix)
		}
		if v.Autonomous && p.Bits() != 64 {
			return fmt.Errorf("autonomous RA prefix %s must be /64", p)
		}
		if v.PreferredLifetimeSeconds > v.ValidLifetimeSeconds {
			return errors.New("RA preferred lifetime exceeds valid lifetime")
		}
		if err := v.ValidUntil.Validate(); err != nil {
			return err
		}
		if err := v.PreferredUntil.Validate(); err != nil {
			return err
		}
		if !v.PreferredUntil.UTC.IsZero() && !v.ValidUntil.UTC.IsZero() && v.PreferredUntil.UTC.After(v.ValidUntil.UTC) {
			return errors.New("RA preferred_until exceeds valid_until")
		}
		if seen[p.String()] {
			return fmt.Errorf("duplicate RA prefix %s", p)
		}
		seen[p.String()] = true
	}
	servers, domains := dnsEntries(c)
	for _, entry := range servers {
		s := entry.Name
		for _, d := range entry.Lifetimes {
			if err := d.Validate(); err != nil {
				return err
			}
		}
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is6() || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() || a.Zone() != "" {
			return fmt.Errorf("invalid RA RDNSS %q", s)
		}
	}
	for _, entry := range domains {
		for _, d := range entry.Lifetimes {
			if err := d.Validate(); err != nil {
				return err
			}
		}
		if _, err := domain(entry.Name); err != nil {
			return err
		}
	}
	maximum, err := maximumPacketSize(c, true)
	if err != nil {
		return err
	}
	if maximum > 1240 {
		return errors.New("RA options exceed the minimum IPv6 MTU; split prefix/DNS intent")
	}
	return nil
}
func domain(s string) ([]byte, error) {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if len(s) == 0 || len(s) > 253 {
		return nil, fmt.Errorf("invalid DNSSL domain %q", s)
	}
	var out []byte
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return nil, fmt.Errorf("invalid DNSSL label in %q", s)
		}
		for _, v := range label {
			if !(v >= 'a' && v <= 'z' || v >= '0' && v <= '9' || v == '-') {
				return nil, fmt.Errorf("DNSSL requires ASCII A-label domain: %q", s)
			}
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0), nil
}
func encode(c Config, hardware net.HardwareAddr, clock boottime.Clock, withdraw bool) ([]byte, error) {
	p := make([]byte, 16)
	p[0] = 134
	p[4] = 64
	if c.Managed {
		p[5] |= 0x80
	}
	if c.OtherConfig {
		p[5] |= 0x40
	}
	switch c.RouterPreference {
	case "high":
		p[5] |= 0x08
	case "low":
		p[5] |= 0x18
	}
	routerLifetime := int(c.Authority.Seconds(uint32(c.RouterLifetimeSeconds), clock))
	if withdraw {
		routerLifetime = 0
	}
	binary.BigEndian.PutUint16(p[6:], uint16(routerLifetime))
	if len(hardware) == 6 {
		p = append(p, 1, 1)
		p = append(p, hardware...)
	}
	if c.MTU != 0 {
		opt := make([]byte, 8)
		opt[0] = 5
		opt[1] = 1
		binary.BigEndian.PutUint32(opt[4:], uint32(c.MTU))
		p = append(p, opt...)
	}
	for _, prefix := range c.Prefixes {
		n, err := netip.ParsePrefix(prefix.Prefix)
		if err != nil {
			return nil, err
		}
		opt := make([]byte, 32)
		opt[0] = 3
		opt[1] = 4
		opt[2] = byte(n.Bits())
		if prefix.OnLink {
			opt[3] |= 0x80
		}
		if prefix.Autonomous {
			opt[3] |= 0x40
		}
		valid := min(prefix.ValidUntil.Seconds(prefix.ValidLifetimeSeconds, clock), c.Authority.Seconds(prefix.ValidLifetimeSeconds, clock))
		preferred := min(prefix.PreferredUntil.Seconds(prefix.PreferredLifetimeSeconds, clock), c.Authority.Seconds(prefix.PreferredLifetimeSeconds, clock))
		if preferred > valid {
			preferred = valid
		}
		if withdraw {
			valid = 0
			preferred = 0
		}
		binary.BigEndian.PutUint32(opt[4:], valid)
		binary.BigEndian.PutUint32(opt[8:], preferred)
		copy(opt[16:], n.Addr().AsSlice())
		p = append(p, opt...)
	}
	return appendDNSOptions(p, c, clock, withdraw)
}

func solicitation(payload []byte, source netip.Addr, hopLimit, ifindex, wantIndex int) bool {
	// RFC 4861 sections 4.1/6.1.1 permit an address assigned to the sender,
	// including a global unicast address. Only advertisements require a
	// link-local source. Hop limit 255 establishes that the RS is on-link.
	if len(payload) < 8 || payload[0] != 133 || payload[1] != 0 || hopLimit != 255 || ifindex != wantIndex || !source.Is6() || source.Is4In6() || source.IsMulticast() {
		return false
	}
	for at := 8; at < len(payload); {
		if at+2 > len(payload) || payload[at+1] == 0 {
			return false
		}
		size := int(payload[at+1]) * 8
		if at+size > len(payload) {
			return false
		}
		if payload[at] == 1 && source.IsUnspecified() {
			return false
		}
		at += size
	}
	return true
}
func jitter(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return min + (max-min)/2
	}
	return min + time.Duration(binary.BigEndian.Uint64(raw[:])%uint64(max-min+1))
}

func Run(ctx context.Context, cfg Config, effects Effects, ready func()) error {
	return RunWithUpdates(ctx, cfg, nil, effects, ready)
}

// RunWithUpdates preserves the socket and link-local source across same-identity
// updates. Invalid updates publish LastUpdateError and retain the active config.
// Closing updates retains the final config; cancellation still withdraws it.
func RunWithUpdates(ctx context.Context, cfg Config, updates <-chan Config, effects Effects, ready func()) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("RA effects are required")
	}
	cfg = normalized(cfg)
	wanted, _ := netip.ParseAddr(cfg.SourceAddress)
	ifi, source, err := linklocal.Wait(ctx, cfg.Interface, wanted)
	if err != nil {
		return err
	}
	if cfg.MTU > ifi.MTU {
		return errors.New("advertised RA MTU exceeds interface MTU")
	}
	fd, packetInfo, err := socket(cfg, ifi, source)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	snapshot := Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, Generation: 1, LastConfigUpdate: time.Now().UTC()}
	pending := withdrawals{}
	var lastSentAt time.Time
	destination := &unix.SockaddrInet6{ZoneId: uint32(ifi.Index)}
	copy(destination.Addr[:], net.ParseIP("ff02::1").To16())
	send := func(updateCtx context.Context, withdraw bool) error {
		// A bridge can select a different MAC when ports join or leave. Do
		// not advertise the constructor's obsolete neighbor-cache address.
		link, err := net.InterfaceByIndex(ifi.Index)
		if err != nil {
			return err
		}
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		withdraw = withdraw || !cfg.ActiveUntil.Live(clock)
		payload, err := encodeWithWithdrawals(cfg, pending, link.HardwareAddr, clock, withdraw)
		if err != nil {
			return err
		}
		if len(payload) > 1240 {
			return errors.New("RA advertisement exceeds the minimum IPv6 MTU")
		}
		if _, err = unix.SendmsgN(fd, payload, packetInfo, destination, 0); err != nil {
			return fmt.Errorf("send RA: %w", err)
		}
		snapshot.Sent++
		lastSentAt = time.Now()
		snapshot.LastSent = lastSentAt.UTC()
		snapshot.Withdrawing = withdraw
		snapshot.WithdrawingPrefixes = pending.prefixNames()
		if withdraw {
			for _, prefix := range cfg.Prefixes {
				snapshot.WithdrawingPrefixes = append(snapshot.WithdrawingPrefixes, prefix.Prefix)
			}
		}
		if !withdraw {
			pending.sent()
		}
		if err = effects.Update(updateCtx, snapshot); err != nil {
			return fmt.Errorf("publish RA status: %w", err)
		}
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := send(cleanup, true); err != nil {
			result = errors.Join(result, fmt.Errorf("withdraw RA: %w", err))
		}
	}()
	if err = send(ctx, false); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	initialMax := time.Duration(cfg.MaxIntervalSeconds) * time.Second
	if initialMax > 16*time.Second {
		initialMax = 16 * time.Second
	}
	nextPeriodic := time.Now().Add(jitter(3*time.Second, initialMax))
	var nextSolicited time.Time
	var nextChanged time.Time
	payload := make([]byte, 2048)
	oob := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case candidate, ok := <-updates:
			if !ok {
				updates = nil
				break
			}
			clock, err := boottime.Read()
			if err != nil {
				return err
			}
			wasExpired := !cfg.ActiveUntil.Live(clock)
			next, withdrawals, changed, err := planUpdate(cfg, candidate, pending, ifi.HardwareAddr, ifi.MTU, clock)
			if err != nil {
				snapshot.RejectedUpdates++
				snapshot.LastUpdateError = err.Error()
				if err := effects.Update(ctx, snapshot); err != nil {
					return fmt.Errorf("publish rejected RA update: %w", err)
				}
				break
			}
			changed = changed || wasExpired && next.ActiveUntil.Live(clock)
			cfg, pending = next, withdrawals
			previousError := snapshot.LastUpdateError
			snapshot.LastUpdateError = ""
			if changed {
				snapshot.Generation++
				snapshot.LastConfigUpdate = time.Now().UTC()
				nextChanged = lastSentAt.Add(3 * time.Second)
				if nextChanged.Before(time.Now()) {
					nextChanged = time.Now()
				}
			}
			if changed || previousError != "" {
				if err := effects.Update(ctx, snapshot); err != nil {
					return fmt.Errorf("publish RA config update: %w", err)
				}
			}
		default:
		}
		if cfg.ActiveUntil != (lifetime.Deadline{}) && !snapshot.Withdrawing {
			clock, err := boottime.Read()
			if err != nil {
				return err
			}
			if !cfg.ActiveUntil.Live(clock) {
				if err := send(ctx, true); err != nil {
					return err
				}
			}
		}
		now := time.Now()
		if !now.Before(nextPeriodic) || !nextSolicited.IsZero() && !now.Before(nextSolicited) || !nextChanged.IsZero() && !now.Before(nextChanged) {
			if err := send(ctx, false); err != nil {
				return err
			}
			nextSolicited = time.Time{}
			nextChanged = time.Time{}
			if pending.any() {
				nextChanged = lastSentAt.Add(3 * time.Second)
			}
			interval := jitter(time.Duration(cfg.MinIntervalSeconds)*time.Second, time.Duration(cfg.MaxIntervalSeconds)*time.Second)
			if snapshot.Sent < 3 && interval > 16*time.Second {
				interval = 16 * time.Second
			}
			nextPeriodic = time.Now().Add(interval)
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		wake := nextPeriodic
		if !nextSolicited.IsZero() && nextSolicited.Before(wake) {
			wake = nextSolicited
		}
		if !nextChanged.IsZero() && nextChanged.Before(wake) {
			wake = nextChanged
		}
		wait := int((time.Until(wake) + time.Millisecond - 1) / time.Millisecond)
		if wait < 0 {
			wait = 0
		}
		if wait > 100 {
			wait = 100
		}
		if _, err := unix.Poll(poll, wait); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, control, flags, source, err := unix.Recvmsg(fd, payload, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return err
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			continue
		}
		from, ok := source.(*unix.SockaddrInet6)
		if !ok {
			continue
		}
		hop, index := 0, 0
		cms, err := unix.ParseSocketControlMessage(oob[:control])
		if err != nil {
			continue
		}
		for _, cm := range cms {
			if cm.Header.Level != unix.IPPROTO_IPV6 {
				continue
			}
			if cm.Header.Type == unix.IPV6_HOPLIMIT && len(cm.Data) >= 4 {
				hop = int(binary.NativeEndian.Uint32(cm.Data))
			}
			if cm.Header.Type == unix.IPV6_PKTINFO && len(cm.Data) >= 20 {
				index = int(binary.NativeEndian.Uint32(cm.Data[16:]))
			}
		}
		if !solicitation(payload[:n], netip.AddrFrom16(from.Addr), hop, index, ifi.Index) {
			continue
		}
		snapshot.LastSolicitation = time.Now().UTC()
		when := time.Now().Add(jitter(0, 500*time.Millisecond))
		earliest := lastSentAt.Add(3 * time.Second)
		if when.Before(earliest) {
			when = earliest
		}
		if nextSolicited.IsZero() || when.Before(nextSolicited) {
			nextSolicited = when
		}
	}
}

func socket(c Config, ifi *net.Interface, source netip.Addr) (int, []byte, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return -1, nil, err
	}
	fail := func(err error) (int, []byte, error) { unix.Close(fd); return -1, nil, err }
	if err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface); err != nil {
		return fail(err)
	}
	for _, s := range []struct{ level, opt, value int }{{unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifi.Index}, {unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255}, {unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, 255}, {unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT, 1}, {unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1}} {
		if err = unix.SetsockoptInt(fd, s.level, s.opt, s.value); err != nil {
			return fail(err)
		}
	}
	var filter unix.ICMPv6Filter
	for i := range filter.Data {
		filter.Data[i] = ^uint32(0)
	}
	filter.Data[133/32] &= ^(uint32(1) << uint(133%32))
	if err = unix.SetsockoptICMPv6Filter(fd, unix.IPPROTO_ICMPV6, unix.ICMPV6_FILTER, &filter); err != nil {
		return fail(err)
	}
	// Do not bind the receive socket to the unicast source: that would filter
	// multicast ff02::2 solicitations. Select the source on each send instead.
	info := &unix.Inet6Pktinfo{Ifindex: uint32(ifi.Index)}
	info.Addr = source.As16()
	var group unix.IPv6Mreq
	group.Interface = uint32(ifi.Index)
	copy(group.Multiaddr[:], net.ParseIP("ff02::2").To16())
	if err = unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, &group); err != nil {
		return fail(err)
	}
	timeout := unix.NsecToTimeval(int64(time.Second))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
		return fail(err)
	}
	return fd, unix.PktInfo6(info), nil
}
