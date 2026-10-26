// SPDX-License-Identifier: BSD-3-Clause
// Package dhcp4 owns a DHCPv4 client protocol and lease journal. It never
// installs addresses or routes: the control plane consumes Effects.Update.
package dhcp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

type Config struct {
	Resource         string `json:"resource"`
	Interface        string `json:"interface"`
	StatePath        string `json:"state_path"`
	Hostname         string `json:"hostname,omitempty"`
	RequestedAddress string `json:"requested_address,omitempty"`
	ClassID          string `json:"class_id,omitempty"`
	ClientID         string `json:"client_id,omitempty"`
}

type Snapshot struct {
	wire.Snapshot
	BootID          string     `json:"boot_id"`
	ObservedNanos   uint64     `json:"observed_nanos"`
	Sequence        uint64     `json:"sequence"`
	Epoch           string     `json:"epoch"`
	LeaseClock      LeaseClock `json:"lease_clock,omitzero"`
	Stopped         bool       `json:"stopped,omitempty"`
	HardwareAddress string     `json:"hardware_address"`
	ClientIdentity  string     `json:"client_identity"`
	Active          bool       `json:"active"`
}
type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" {
		return errors.New("dhcp4 resource is required")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") {
		return errors.New("dhcp4 interface is invalid")
	}
	if !filepath.IsAbs(c.StatePath) {
		return errors.New("dhcp4 state_path must be absolute")
	}
	for name, v := range map[string]string{"hostname": c.Hostname, "class_id": c.ClassID, "client_id": c.ClientID} {
		if len(v) > 255 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("dhcp4 %s must fit one DHCP option", name)
		}
	}
	if c.RequestedAddress != "" {
		a, err := netip.ParseAddr(c.RequestedAddress)
		if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
			return errors.New("dhcp4 requested_address must be a unicast IPv4 address")
		}
	}
	return nil
}

type packetConn interface {
	ReadFromUDP([]byte) (int, *net.UDPAddr, error)
	WriteToUDP([]byte, *net.UDPAddr) (int, error)
	SetReadDeadline(time.Time) error
	Close() error
}

type client struct {
	config                               Config
	hardware                             net.HardwareAddr
	conn                                 packetConn
	effects                              Effects
	readClock                            func() (bfd.Clock, error)
	clock                                bfd.Clock
	xid                                  func() uint32
	state                                wire.State
	lease                                wire.Lease
	selected                             wire.Message
	transaction                          uint32
	nextRetry                            int64
	retry                                time.Duration
	phaseStarted                         int64
	reboot                               bool
	persist                              func(Snapshot) error
	leaseRenew, leaseRebind, leaseExpiry int64
	leaseClock                           LeaseClock
	sequence                             uint64
	epoch                                string
}

// refreshClock is separate from UTC display metadata. BOOTTIME includes time
// spent suspended, so a stalled client must expire its lease before receiving
// queued traffic. A clock failure never falls back to wall-clock authority.
func (c *client) refreshClock() error {
	if c.readClock == nil {
		return errors.New("DHCPv4 boot clock is required")
	}
	clock, err := c.readClock()
	if err != nil {
		return err
	}
	if clock.BootID == "" || clock.BoottimeNanoseconds <= 0 || clock.BoottimeNanoseconds > math.MaxInt64-int64(65*time.Second) || clock.WallTime.IsZero() || clock.WallTime.Year() < 1970 || clock.WallTime.Year() > 9999 || c.clock.BootID != "" && (clock.BootID != c.clock.BootID || clock.BoottimeNanoseconds < c.clock.BoottimeNanoseconds) {
		return errors.New("invalid or changed DHCPv4 boot clock")
	}
	c.clock = clock
	return nil
}

func (c *client) leaseTimers(renew, rebind, expiry time.Duration) error {
	now := c.clock.BoottimeNanoseconds
	if c.sequence == math.MaxUint64 || expiry <= 0 || expiry > time.Duration(math.MaxInt64-now) || renew <= 0 || rebind <= renew || expiry <= rebind {
		return errors.New("invalid DHCPv4 boot lease interval")
	}
	c.leaseRenew, c.leaseRebind, c.leaseExpiry = now+int64(renew), now+int64(rebind), now+int64(expiry)
	c.leaseClock = LeaseClock{BootID: c.clock.BootID, Epoch: c.epoch, Sequence: c.sequence + 1, AcquiredNanos: uint64(now), RenewNanos: uint64(c.leaseRenew), RebindNanos: uint64(c.leaseRebind), ExpiresNanos: uint64(c.leaseExpiry)}
	return nil
}

func (c *client) snapshot() Snapshot {
	now := c.clock.WallTime.UTC()
	active := (c.state == wire.StateBound || c.state == wire.StateRenewing || c.state == wire.StateRebinding) && c.lease.Address.IsValid() && c.clock.BoottimeNanoseconds < c.leaseExpiry
	return Snapshot{Snapshot: wire.SnapshotFromLease(c.config.Resource, c.config.Interface, c.state, c.lease, now), BootID: c.clock.BootID, Epoch: c.epoch, Sequence: c.sequence, ObservedNanos: uint64(c.clock.BoottimeNanoseconds), LeaseClock: c.leaseClock, HardwareAddress: c.hardware.String(), ClientIdentity: c.config.ClientID, Active: active}
}
func (c *client) publish(ctx context.Context) error {
	if c.sequence == math.MaxUint64 {
		return errors.New("DHCPv4 observation sequence exhausted")
	}
	c.sequence++
	s := c.snapshot()
	if err := ValidateSnapshot(c.config, s); err != nil {
		return err
	}
	if err := c.persist(s); err != nil {
		return fmt.Errorf("persist DHCPv4 lease: %w", err)
	}
	if err := c.effects.Update(ctx, s); err != nil {
		return fmt.Errorf("publish DHCPv4 lease: %w", err)
	}
	if s.Active {
		if err := c.refreshClock(); err != nil {
			return err
		}
		if !CurrentLease(s, c.clock) {
			return errors.New("DHCPv4 lease acknowledgment arrived after original boot expiry")
		}
	}
	return nil
}

func (c *client) clearLease() {
	c.lease = wire.Lease{}
	c.leaseClock = LeaseClock{}
	c.leaseRenew, c.leaseRebind, c.leaseExpiry = 0, 0, 0
}
func (c *client) start(ctx context.Context, previous *Snapshot) error {
	if err := c.refreshClock(); err != nil {
		return err
	}
	if c.epoch == "" {
		epoch, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		c.epoch = epoch.String()
	}
	now := c.clock.WallTime
	// Active describes current kernel intent, not whether the server's lease
	// can be requested again. In particular INIT-REBOOT persists an inactive
	// Requesting snapshot; another restart must not forget that lease.
	if previous != nil && previous.Resource == c.config.Resource && previous.Interface == c.config.Interface && previous.HardwareAddress == c.hardware.String() && previous.ClientIdentity == c.config.ClientID {
		remaining := previous.ExpiresAt.Sub(now)
		eligible := !previous.AcquiredAt.After(now) && remaining > 0
		if previous.LeaseClock != (LeaseClock{}) {
			if err := ValidateSnapshot(c.config, *previous); err != nil {
				return err
			}
			if previous.LeaseClock.BootID == c.clock.BootID {
				remaining = time.Duration(previous.LeaseClock.ExpiresNanos) - time.Duration(c.clock.BoottimeNanoseconds)
				eligible = remaining > 0
			}
		}
		lease := wire.LeaseFromSnapshot(previous.Snapshot)
		if eligible && lease.Address.Is4() && !lease.Address.IsUnspecified() && !lease.Address.IsMulticast() && lease.PrefixLength >= 0 && lease.PrefixLength <= 32 && lease.ServerID.Is4() && !lease.ServerID.IsUnspecified() && !lease.ServerID.IsMulticast() && lease.LeaseTime > 0 {
			c.lease = lease
			c.leaseClock = previous.LeaseClock
			// The saved lease is only an INIT-REBOOT hint. Limit its request
			// lifetime once; a fresh server ACK establishes actual authority.
			if remaining > time.Duration(math.MaxInt64-c.clock.BoottimeNanoseconds) {
				return errors.New("DHCPv4 reboot hint exceeds boot clock")
			}
			c.leaseExpiry = c.clock.BoottimeNanoseconds + int64(remaining)
			c.reboot = true
			return c.begin(ctx, wire.StateRequesting)
		}
	}
	c.clearLease()
	return c.begin(ctx, wire.StateDiscovering)
}
func (c *client) begin(ctx context.Context, state wire.State) error {
	c.state = state
	c.transaction = c.xid()
	c.retry = 4 * time.Second
	c.phaseStarted = c.clock.BoottimeNanoseconds
	if err := c.publish(ctx); err != nil {
		return err
	}
	return c.send(ctx)
}
func (c *client) send(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := wire.Config{Resource: c.config.Resource, Interface: c.config.Interface, HardwareAddr: c.hardware, Hostname: c.config.Hostname, ClassID: c.config.ClassID, ClientID: c.config.ClientID}
	if c.state == wire.StateDiscovering {
		cfg.RequestedAddress = c.config.RequestedAddress
	}
	opts := wire.RequestOptions(cfg, wire.Message{})
	typ := byte(wire.MessageRequest)
	target := net.IPv4bcast
	switch c.state {
	case wire.StateDiscovering:
		typ = wire.MessageDiscover
	case wire.StateRequesting:
		if c.reboot {
			opts[wire.OptionRequestedIP] = c.lease.Address.AsSlice()
		} else {
			opts[wire.OptionRequestedIP] = c.selected.YIAddr.AsSlice()
			opts[wire.OptionServerID] = append([]byte(nil), c.selected.Options[wire.OptionServerID]...)
		}
	case wire.StateRenewing:
		target = net.IP(c.lease.ServerID.AsSlice())
	case wire.StateRebinding:
	default:
		return fmt.Errorf("cannot send DHCPv4 request in state %s", c.state)
	}
	// Adapt routerd's request encoding, including every value this client
	// requires in an ACK. dnsmasq omits an unrequested custom subnet mask.
	payload := make([]byte, 240)
	payload[0], payload[1], payload[2] = 1, 1, byte(len(c.hardware))
	binary.BigEndian.PutUint32(payload[4:8], c.transaction)
	binary.BigEndian.PutUint16(payload[10:12], 0x8000)
	copy(payload[28:44], c.hardware)
	copy(payload[236:240], []byte{99, 130, 83, 99})
	payload = append(payload, wire.OptionMessageType, 1, typ, wire.OptionParameterRequest, 12,
		wire.OptionSubnetMask, wire.OptionLeaseTime, wire.OptionRouter, wire.OptionDNSServer,
		wire.OptionHostname, wire.OptionDomainName, wire.OptionMTU, wire.OptionBroadcastAddress,
		wire.OptionNTPServer, wire.OptionTFTPServer, wire.OptionBootfile, wire.OptionServerID)
	for code, value := range opts {
		payload = append(payload, code, byte(len(value)))
		payload = append(payload, value...)
	}
	payload = append(payload, wire.OptionEnd)
	for len(payload) < 300 {
		payload = append(payload, 0)
	}
	if c.state == wire.StateRenewing || c.state == wire.StateRebinding {
		copy(payload[12:16], c.lease.Address.AsSlice())
		if c.state == wire.StateRenewing {
			binary.BigEndian.PutUint16(payload[10:12], 0)
		}
	}
	seconds := int((c.clock.BoottimeNanoseconds - c.phaseStarted) / int64(time.Second))
	if seconds > 65535 {
		seconds = 65535
	}
	if seconds < 0 {
		seconds = 0
	}
	binary.BigEndian.PutUint16(payload[8:10], uint16(seconds))
	if _, err := c.conn.WriteToUDP(payload, &net.UDPAddr{IP: target, Port: 67}); err != nil {
		return fmt.Errorf("send DHCPv4: %w", err)
	}
	// RFC 2131 randomized exponential retransmission, initially 4 +/- 1s.
	variation := time.Duration(wire.NewXID()%2000000001) - time.Second
	c.nextRetry = c.clock.BoottimeNanoseconds + int64(c.retry+variation)
	if c.retry < 64*time.Second {
		c.retry *= 2
	}
	return nil
}
func (c *client) tick(ctx context.Context) error {
	if err := c.refreshClock(); err != nil {
		return err
	}
	now := c.clock.BoottimeNanoseconds
	if c.lease.Address.IsValid() && now >= c.leaseExpiry {
		c.state = wire.StateExpired
		c.clearLease()
		c.reboot = false
		if err := c.publish(ctx); err != nil {
			return err
		}
		return c.begin(ctx, wire.StateDiscovering)
	}
	if (c.state == wire.StateBound || c.state == wire.StateRenewing) && now >= c.leaseRebind {
		return c.begin(ctx, wire.StateRebinding)
	}
	if c.state == wire.StateBound && now >= c.leaseRenew {
		return c.begin(ctx, wire.StateRenewing)
	}
	if c.state == wire.StateBound {
		return nil
	}
	if c.state == wire.StateRequesting && now-c.phaseStarted >= int64(60*time.Second) {
		c.reboot = false
		c.clearLease()
		return c.begin(ctx, wire.StateDiscovering)
	}
	if now >= c.nextRetry {
		return c.send(ctx)
	}
	return nil
}

// decode guards the imported decoder against invalid hlen and incomplete TLVs.
// AF_PACKET bypasses UDP socket validation, so every consumed field is checked.
func decode(payload []byte) (wire.Message, error) {
	if len(payload) < 240 || payload[0] != 2 || payload[1] != 1 || payload[2] != 6 {
		return wire.Message{}, errors.New("invalid DHCPv4 Ethernet reply header")
	}
	m, err := wire.Decode(payload)
	if err != nil {
		return wire.Message{}, err
	}
	// RFC 3396 fragments are concatenated, including overloaded file/sname
	// fields. The original wire decoder keeps only the last fragment.
	m.Options = map[byte][]byte{}
	parse := func(area []byte) error {
		for i := 0; i < len(area); {
			code := area[i]
			i++
			if code == 0 {
				continue
			}
			if code == 255 {
				return nil
			}
			if i >= len(area) {
				return errors.New("truncated DHCPv4 option length")
			}
			n := int(area[i])
			i++
			if i+n > len(area) {
				return errors.New("truncated DHCPv4 option")
			}
			m.Options[code] = append(m.Options[code], area[i:i+n]...)
			i += n
		}
		return nil
	}
	if err := parse(payload[240:]); err != nil {
		return wire.Message{}, err
	}
	if overload, ok := m.Options[52]; ok {
		if len(overload) != 1 || overload[0] == 0 || overload[0] > 3 {
			return wire.Message{}, errors.New("invalid DHCPv4 option overload")
		}
		if overload[0]&1 != 0 {
			if err := parse(payload[108:236]); err != nil {
				return wire.Message{}, err
			}
		}
		if overload[0]&2 != 0 {
			if err := parse(payload[44:108]); err != nil {
				return wire.Message{}, err
			}
		}
	}
	for code, size := range map[byte]int{wire.OptionMessageType: 1, wire.OptionServerID: 4, wire.OptionSubnetMask: 4, wire.OptionLeaseTime: 4} {
		if b, exists := m.Options[code]; exists && len(b) != size {
			return wire.Message{}, fmt.Errorf("invalid DHCPv4 option %d length", code)
		}
	}
	return m, nil
}
func (c *client) handle(ctx context.Context, payload []byte) error {
	if err := c.refreshClock(); err != nil {
		return err
	}
	// A reply queued before a stall cannot renew an already expired lease or
	// bypass the new discovery transaction. Tick withdraws before decoding it.
	if c.lease.Address.IsValid() && c.clock.BoottimeNanoseconds >= c.leaseExpiry {
		return c.tick(ctx)
	}
	m, err := decode(payload)
	if err != nil {
		return nil
	}
	if m.XID != c.transaction || !bytes.Equal(m.CHAddr, c.hardware) {
		return nil
	}
	sid := m.Options[wire.OptionServerID]
	if len(sid) != 4 {
		return nil
	}
	server := netip.AddrFrom4([4]byte{sid[0], sid[1], sid[2], sid[3]})
	if server.IsUnspecified() || server.IsMulticast() {
		return nil
	}
	if c.state == wire.StateRequesting && !c.reboot && !bytes.Equal(sid, c.selected.Options[wire.OptionServerID]) {
		return nil
	}
	if c.state == wire.StateRenewing && server != c.lease.ServerID {
		return nil
	}
	switch m.MessageType() {
	case wire.MessageOffer:
		if c.state != wire.StateDiscovering || !m.YIAddr.Is4() || m.YIAddr.IsUnspecified() || m.YIAddr.IsMulticast() {
			return nil
		}
		c.selected = m
		c.state = wire.StateRequesting
		c.retry = 4 * time.Second
		c.phaseStarted = c.clock.BoottimeNanoseconds
		// Selecting/requesting is one transaction and keeps the discovery xid.
		if err := c.publish(ctx); err != nil {
			return err
		}
		return c.send(ctx)
	case wire.MessageNAK:
		if c.state != wire.StateRequesting && c.state != wire.StateRenewing && c.state != wire.StateRebinding {
			return nil
		}
		c.state = wire.StateExpired
		c.clearLease()
		c.reboot = false
		if err := c.publish(ctx); err != nil {
			return err
		}
		return c.begin(ctx, wire.StateDiscovering)
	case wire.MessageACK:
		if c.state != wire.StateRequesting && c.state != wire.StateRenewing && c.state != wire.StateRebinding {
			return nil
		}
		if m.YIAddr.IsUnspecified() && c.lease.Address.Is4() {
			m.YIAddr = c.lease.Address
		}
		if !m.YIAddr.Is4() || m.YIAddr.IsUnspecified() || m.YIAddr.IsMulticast() {
			return nil
		}
		if c.state == wire.StateRequesting && !c.reboot && m.YIAddr != c.selected.YIAddr {
			return nil
		}
		if c.reboot && m.YIAddr != c.lease.Address {
			return nil
		}
		mask := m.Options[wire.OptionSubnetMask]
		if len(mask) != 4 {
			return nil
		}
		ones, bits := net.IPMask(mask).Size()
		if bits != 32 || ones < 0 {
			return nil
		}
		life := m.Options[wire.OptionLeaseTime]
		if len(life) != 4 || binary.BigEndian.Uint32(life) == 0 {
			return nil
		}
		lease := wire.LeaseFromACK(m, c.clock.WallTime.UTC())
		if lease.T1 <= 0 || lease.T2 <= lease.T1 || lease.T2 >= lease.LeaseTime {
			return nil
		}
		if err := c.leaseTimers(lease.T1, lease.T2, lease.LeaseTime); err != nil {
			return err
		}
		c.lease = lease
		c.state = wire.StateBound
		c.reboot = false
		return c.publish(ctx)
	}
	return nil
}
