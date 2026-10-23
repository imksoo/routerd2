//go:build linux

// SPDX-License-Identifier: BSD-3-Clause
// Package dhcp6pd supervises the pinned routerd IA_PD state machine with native
// Linux UDP transport, durable identity and typed lease effects.
package dhcp6pd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/imksoo/routerd/pkg/pdclient"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type Config struct {
	Resource        string `json:"resource"`
	Interface       string `json:"interface"`
	StatePath       string `json:"state_path"`
	ClientDUID      string `json:"client_duid,omitempty"`
	IAID            uint32 `json:"iaid"`
	PrefixLength    int    `json:"prefix_length,omitempty"`
	InformationOnly bool   `json:"information_only,omitempty"`
}
type Snapshot struct {
	pdclient.Snapshot
	Active                    bool      `json:"active"`
	InformationOnly           bool      `json:"information_only,omitempty"`
	InformationRefreshSeconds uint32    `json:"information_refresh_seconds,omitempty"`
	InformationRefreshAt      time.Time `json:"information_refresh_at,omitempty"`
	InformationStale          bool      `json:"information_stale,omitempty"`
}
type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }
func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" {
		return errors.New("dhcp6pd resource is required")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") {
		return errors.New("dhcp6pd interface is invalid")
	}
	if !filepath.IsAbs(c.StatePath) {
		return errors.New("dhcp6pd state_path must be absolute")
	}
	if c.PrefixLength < 0 || c.PrefixLength > 128 {
		return errors.New("dhcp6pd prefix_length must be 1..128 or zero for the default /60")
	}
	if c.ClientDUID != "" {
		if _, err := parseDUID(c.ClientDUID); err != nil {
			return err
		}
	}
	return nil
}
func parseDUID(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) < 4 || len(b) > 128 {
		return nil, errors.New("client_duid must be 4..128 bytes of hexadecimal DUID")
	}
	return b, nil
}

type transport struct {
	conn interface {
		SetWriteDeadline(time.Time) error
		WriteToUDP([]byte, *net.UDPAddr) (int, error)
	}
	interfaceName    string
	ifindex          int
	prefixLength     int
	now              func() time.Time
	last             pdclient.OutboundPacket
	exchangeStart    time.Time
	informationLast  pdclient.OutboundPacket
	informationStart time.Time
	requestRetry     time.Time
	requestTimeout   time.Duration
	requestCount     int
	informationXID   uint32
}

func (t *transport) Send(ctx context.Context, p pdclient.OutboundPacket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Message.Type != pdclient.MessageRelease {
		p.Message.ORO = []uint16{23, 24, 31, 64}
		if p.Message.Type == pdclient.MessageInfoRequest {
			p.Message.ORO = append(p.Message.ORO, 32)
		}
	}
	// RFC 8415 21.21/21.22: clients leave lease/timer selection to the
	// server. Keep only the prefix hint in outbound IA_PD/IAPREFIX options.
	p.Message.T1, p.Message.T2 = 0, 0
	p.Message.Preferred, p.Message.Valid = 0, 0
	if p.Message.Type == pdclient.MessageSolicit && !p.Message.Prefix.IsValid() {
		p.Message.Prefix = netip.PrefixFrom(netip.IPv6Unspecified(), t.prefixLength)
	}
	if p.Message.Type == pdclient.MessageInfoRequest {
		if p.Message.TransactionID != t.informationXID {
			t.informationStart = t.now()
		}
	} else if p.Message.TransactionID != t.last.Message.TransactionID || p.Message.Type != t.last.Message.Type {
		t.exchangeStart = t.now()
		t.requestTimeout = time.Second
		t.requestCount = 0
	}
	data, err := pdclient.EncodeMessage(p.Message)
	if err != nil {
		return err
	}
	// The imported codec writes elapsed-time zero. Set it for retransmissions.
	elapsed := int(t.now().Sub(t.exchangeStart) / (10 * time.Millisecond))
	if p.Message.Type == pdclient.MessageInfoRequest {
		elapsed = int(t.now().Sub(t.informationStart) / (10 * time.Millisecond))
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > 65535 {
		elapsed = 65535
	}
	for at := 4; at+4 <= len(data); {
		code := binary.BigEndian.Uint16(data[at:])
		n := int(binary.BigEndian.Uint16(data[at+2:]))
		at += 4
		if at+n > len(data) {
			return errors.New("invalid encoded DHCPv6 options")
		}
		if code == 8 && n == 2 {
			binary.BigEndian.PutUint16(data[at:], uint16(elapsed))
		}
		at += n
	}
	p.Payload = data
	if err = t.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err = t.conn.WriteToUDP(data, &net.UDPAddr{IP: net.ParseIP("ff02::1:2"), Port: 547, Zone: t.interfaceName}); err != nil {
		return err
	}
	if p.Message.Type == pdclient.MessageInfoRequest {
		t.informationLast = p
	} else {
		t.last = p
	}
	if p.Message.Type == pdclient.MessageRequest {
		t.requestCount++
		t.requestRetry = t.now().Add(t.requestTimeout)
		if t.requestTimeout < 30*time.Second {
			t.requestTimeout *= 2
			if t.requestTimeout > 30*time.Second {
				t.requestTimeout = 30 * time.Second
			}
		}
	}
	if p.Message.Type == pdclient.MessageInfoRequest {
		t.informationXID = p.Message.TransactionID
	}
	return nil
}

func validReply(m pdclient.Message, c *pdclient.Client, informationXID uint32) (pdclient.Message, bool) {
	if (m.Type != pdclient.MessageAdvertise && m.Type != pdclient.MessageReply) || len(m.ServerDUID) < 4 || len(m.ServerDUID) > 128 || !bytes.Equal(m.ClientDUID, c.Config.ClientDUID) {
		return m, false
	}
	if informationXID != 0 && m.TransactionID == informationXID && m.Type == pdclient.MessageReply {
		return m, true
	}
	if m.IAID != c.Config.IAID || !m.Prefix.IsValid() || !m.Prefix.Addr().Is6() || m.Prefix.Addr().Is4In6() || m.Prefix.Addr().IsUnspecified() || m.Prefix.Addr().IsMulticast() || m.Prefix.Addr().IsLinkLocalUnicast() || m.Prefix != m.Prefix.Masked() || m.Preferred > m.Valid {
		return m, false
	}
	if c.State == pdclient.StateRenewing && !bytes.Equal(m.ServerDUID, c.Lease.ServerDUID) {
		return m, false
	}
	if m.Valid == 0 {
		return m, true
	} // Valid zero is an explicit withdrawal.
	if m.T1 != 0 && m.T2 != 0 && m.T1 > m.T2 {
		return m, false
	}
	// Equality and timers beyond a prefix's validity are legal. Expiry is
	// checked independently by the client before renewal/rebinding. Choose
	// timers only for zero fields, as required by RFC 8415 section 21.21.
	basis := m.Preferred
	if basis == 0 {
		basis = m.Valid
	}
	if m.T1 == 0 {
		m.T1 = max(1, basis/2)
		if m.T2 != 0 && m.T1 > m.T2 {
			m.T1 = m.T2
		}
	}
	if m.T2 == 0 {
		m.T2 = max(m.T1, uint32(uint64(basis)*4/5))
	}
	return m, true
}

const defaultInformationRefresh = uint32(86400)

// The refresh timer is not a lease lifetime. Keep the learned information
// while retrying, but expose staleness to the policy/compiler instead of
// silently interpreting a missing refresh as permission to extend a lease.
func informationRefresh(payload []byte) (uint32, bool) {
	value := defaultInformationRefresh
	found := false
	if len(payload) < 4 {
		return 0, false
	}
	for options := payload[4:]; len(options) != 0; {
		if len(options) < 4 {
			return 0, false
		}
		code, n := binary.BigEndian.Uint16(options), int(binary.BigEndian.Uint16(options[2:]))
		options = options[4:]
		if n > len(options) {
			return 0, false
		}
		if code == 32 {
			if found || n != 4 {
				return 0, false
			}
			found = true
			value = max(600, binary.BigEndian.Uint32(options))
		}
		options = options[n:]
	}
	return value, true
}

func refreshAt(updated time.Time, seconds uint32) time.Time {
	if updated.IsZero() || seconds == ^uint32(0) {
		return time.Time{}
	}
	if seconds == 0 {
		seconds = defaultInformationRefresh
	}
	return updated.Add(time.Duration(seconds) * time.Second)
}

// Run uses one socket and one IA_PD identity per interface. A control-plane
// compiler should merge DHCPv6Information intent into its PD worker when both
// refer to the same interface, rather than start competing UDP/546 clients.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("dhcp6pd effects are required")
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
		return fmt.Errorf("DHCPv6 state is already owned: %w", err)
	}
	var previous Snapshot
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err = json.Unmarshal(data, &previous); err != nil {
			return fmt.Errorf("decode DHCPv6 state: %w", err)
		}
		if previous.Resource != cfg.Resource || previous.Interface != cfg.Interface {
			return errors.New("DHCPv6 journal belongs to another resource/interface")
		}
	}
	ifi, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return err
	}
	identity := cfg.ClientDUID
	if identity == "" && previous.ClientDUID != "" {
		identity = previous.ClientDUID
	}
	var duid []byte
	if identity != "" {
		duid, err = parseDUID(identity)
		if err != nil {
			return err
		}
	} else {
		if len(ifi.HardwareAddr) != 6 {
			return errors.New("automatic DHCPv6 DUID-LL requires Ethernet; configure client_duid")
		}
		duid = append([]byte{0, 3, 0, 1}, ifi.HardwareAddr...)
	}
	conn, err := listen(cfg.Interface, ifi.Index)
	if err != nil {
		return err
	}
	defer conn.Close()
	t := &transport{conn: conn, interfaceName: cfg.Interface, ifindex: ifi.Index, prefixLength: cfg.PrefixLength, now: time.Now}
	c, err := pdclient.New(pdclient.Config{Resource: cfg.Resource, Interface: cfg.Interface, ClientDUID: duid, IAID: cfg.IAID, WantPrefix: cfg.PrefixLength, Transaction: transaction}, t)
	if err != nil {
		return err
	}
	if len(data) > 0 && strings.EqualFold(previous.ClientDUID, hex.EncodeToString(duid)) && previous.IAID == cfg.IAID {
		c.Restore(previous.Snapshot)
		if !cfg.InformationOnly {
			restorePrevious(c, previous.Snapshot, time.Now())
		}
		if cfg.InformationOnly {
			c.Lease = pdclient.Lease{}
			c.PreviousPrefixes = nil
		}
		if cfg.InformationOnly || c.State == pdclient.StateRequesting || c.State == pdclient.StateSoliciting {
			c.State = pdclient.StateIdle
		}
	}
	infoRefresh := previous.InformationRefreshSeconds
	if infoRefresh == 0 {
		infoRefresh = defaultInformationRefresh
	}
	var last Snapshot
	defer func() {
		if last.Resource == "" {
			return
		}
		withdraw := last
		withdraw.Active = false
		withdraw.CurrentPrefix = ""
		withdraw.PreviousPrefixes = nil
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := effects.Update(cleanup, withdraw); err != nil {
			result = errors.Join(result, fmt.Errorf("withdraw DHCPv6 effects: %w", err))
		}
	}()
	publish := func(force bool) error {
		s := Snapshot{Snapshot: c.Snapshot(), InformationOnly: cfg.InformationOnly, InformationRefreshSeconds: infoRefresh}
		s.InformationRefreshAt = refreshAt(s.InfoUpdatedAt, infoRefresh)
		s.InformationStale = !s.InformationRefreshAt.IsZero() && !time.Now().Before(s.InformationRefreshAt)
		s.Active = s.CurrentPrefix != "" && (s.State == pdclient.StateBound || s.State == pdclient.StateRenewing || s.State == pdclient.StateRebinding) && time.Now().Before(s.ExpiresAt)
		if cfg.InformationOnly {
			s.Active = !s.InfoUpdatedAt.IsZero()
		}
		old, cur := last, s
		old.UpdatedAt = time.Time{}
		cur.UpdatedAt = time.Time{}
		if !force && reflect.DeepEqual(old, cur) {
			return nil
		}
		if err := writeSnapshot(cfg.StatePath, s); err != nil {
			return fmt.Errorf("persist DHCPv6 state: %w", err)
		}
		if err := effects.Update(ctx, s); err != nil {
			return fmt.Errorf("publish DHCPv6 state: %w", err)
		}
		last = s
		return nil
	}
	if err = publish(true); err != nil {
		return err
	}
	if cfg.InformationOnly {
		err = c.InfoRequest(ctx)
	} else if c.Lease.Prefix.IsValid() {
		err = c.Renew(ctx)
	} else {
		err = c.Start(ctx)
	}
	if err != nil {
		return err
	}
	if err = publish(false); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	informationPending := cfg.InformationOnly
	nextInfo := time.Now().Add(time.Second)
	if !cfg.InformationOnly {
		nextInfo = time.Time{}
	}
	infoRetry := time.Second
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		if !nextInfo.IsZero() && !now.Before(nextInfo) {
			if informationPending {
				err = t.Send(ctx, t.informationLast)
			} else {
				err = c.InfoRequest(ctx)
				informationPending = true
				infoRetry = time.Second
			}
			if err != nil {
				return err
			}
			if infoRetry < 120*time.Second {
				infoRetry *= 2
			}
			nextInfo = now.Add(infoRetry)
		}
		if !cfg.InformationOnly {
			if err := c.Tick(ctx); err != nil {
				return err
			}
			if c.State == pdclient.StateRequesting && !now.Before(t.requestRetry) {
				if t.requestCount >= 10 {
					if err := c.Start(ctx); err != nil {
						return err
					}
				} else if err := t.Send(ctx, t.last); err != nil {
					return err
				}
			}
		}
		if err := publish(false); err != nil {
			return err
		}
		if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
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
		if from == nil || from.Port != 547 {
			continue
		}
		if !statusOK(buf[:n]) {
			continue
		}
		m, err := pdclient.DecodeMessage(buf[:n])
		if err != nil {
			continue
		}
		isInformation := informationPending && m.TransactionID == t.informationXID && m.Type == pdclient.MessageReply
		var replyRefresh uint32
		if isInformation {
			var ok bool
			replyRefresh, ok = informationRefresh(buf[:n])
			if !ok {
				continue
			}
		}
		if !isInformation && (cfg.InformationOnly || m.TransactionID != t.last.Message.TransactionID) {
			continue
		}
		if !isInformation && c.State == pdclient.StateRequesting && !bytes.Equal(m.ServerDUID, t.last.Message.ServerDUID) {
			continue
		}
		infoXID := uint32(0)
		if isInformation {
			infoXID = t.informationXID
		}
		m, ok := validReply(m, c, infoXID)
		if !ok {
			continue
		}
		before := c.Snapshot()
		if err := c.HandleMessage(ctx, m); err != nil {
			return err
		}
		if isInformation && !c.Info.UpdatedAt.IsZero() {
			infoRefresh = replyRefresh
		}
		if !cfg.InformationOnly && c.State == pdclient.StateBound && c.Lease.Valid == 0 {
			c.State = pdclient.StateExpired
			c.Lease = pdclient.Lease{}
		}
		if err := publish(false); err != nil {
			return err
		}
		if isInformation && !c.Info.UpdatedAt.IsZero() {
			informationPending = false
			nextInfo = refreshAt(c.Info.UpdatedAt, infoRefresh)
			infoRetry = time.Second
		}
		if !cfg.InformationOnly && c.State == pdclient.StateBound && c.Snapshot().AcquiredAt != before.AcquiredAt {
			if err := c.InfoRequest(ctx); err != nil {
				return err
			}
			informationPending = true
			nextInfo = time.Now().Add(time.Second)
			infoRetry = time.Second
		}
	}
}

// The imported Restore clears previous prefixes when there is no current
// prefix. Preserve separately valid old prefixes across that restart case.
func restorePrevious(c *pdclient.Client, s pdclient.Snapshot, now time.Time) {
	valid := map[netip.Prefix]time.Time{}
	for _, v := range s.PreviousPrefixes {
		p, err := netip.ParsePrefix(v.Prefix)
		if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Addr().IsLinkLocalUnicast() || p != p.Masked() || p == c.Lease.Prefix || !now.Before(v.ExpiresAt) {
			continue
		}
		valid[p] = v.ExpiresAt
	}
	c.PreviousPrefixes = nil
	for p, until := range valid {
		c.PreviousPrefixes = append(c.PreviousPrefixes, pdclient.PreviousPrefix{Prefix: p, ExpiresAt: until})
	}
	sort.Slice(c.PreviousPrefixes, func(i, j int) bool {
		return c.PreviousPrefixes[i].Prefix.String() < c.PreviousPrefixes[j].Prefix.String()
	})
}

func transaction() (uint32, error) {
	var b [3]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		x := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
		if x != 0 {
			return x, nil
		}
	}
}

// Reject non-success Status Code options, including nested IA_PD/IA_PREFIX.
// The imported Message intentionally does not expose these option fields.
func statusOK(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	var walk func([]byte) bool
	walk = func(options []byte) bool {
		for len(options) > 0 {
			if len(options) < 4 {
				return false
			}
			code := binary.BigEndian.Uint16(options)
			n := int(binary.BigEndian.Uint16(options[2:]))
			options = options[4:]
			if n > len(options) {
				return false
			}
			value := options[:n]
			options = options[n:]
			switch code {
			case 13:
				if len(value) < 2 || binary.BigEndian.Uint16(value) != 0 {
					return false
				}
			case 25:
				if len(value) < 12 || !walk(value[12:]) {
					return false
				}
			case 26:
				if len(value) < 25 || !walk(value[25:]) {
					return false
				}
			}
		}
		return true
	}
	return walk(payload[4:])
}

func listen(name string, index int) (*net.UDPConn, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	var source string
	for _, a := range addrs {
		if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			source = a.IP.String()
			break
		}
	}
	if source == "" {
		return nil, errors.New("DHCPv6 interface has no usable link-local address")
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var inner error
		err := c.Control(func(fd uintptr) {
			for _, setting := range []struct{ level, opt, value int }{{unix.SOL_SOCKET, unix.SO_REUSEADDR, 1}, {unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, index}, {unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 1}} {
				if inner = unix.SetsockoptInt(int(fd), setting.level, setting.opt, setting.value); inner != nil {
					return
				}
			}
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		})
		if err != nil {
			return err
		}
		return inner
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp6", net.JoinHostPort(source+"%"+name, "546"))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
func writeSnapshot(path string, s Snapshot) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp6-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
