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
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
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
	HardwareAddress string `json:"hardware_address"`
	ClientIdentity  string `json:"client_identity"`
	Active          bool   `json:"active"`
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
	config       Config
	hardware     net.HardwareAddr
	conn         packetConn
	effects      Effects
	now          func() time.Time
	xid          func() uint32
	state        wire.State
	lease        wire.Lease
	selected     wire.Message
	transaction  uint32
	nextRetry    time.Time
	retry        time.Duration
	phaseStarted time.Time
	reboot       bool
	persist      func(Snapshot) error
}

func (c *client) snapshot() Snapshot {
	now := c.now().UTC()
	active := (c.state == wire.StateBound || c.state == wire.StateRenewing || c.state == wire.StateRebinding) && c.lease.Address.IsValid() && now.Before(c.lease.ExpiresAt())
	return Snapshot{Snapshot: wire.SnapshotFromLease(c.config.Resource, c.config.Interface, c.state, c.lease, now), HardwareAddress: c.hardware.String(), ClientIdentity: c.config.ClientID, Active: active}
}
func (c *client) publish(ctx context.Context) error {
	s := c.snapshot()
	if err := c.persist(s); err != nil {
		return fmt.Errorf("persist DHCPv4 lease: %w", err)
	}
	if err := c.effects.Update(ctx, s); err != nil {
		return fmt.Errorf("publish DHCPv4 lease: %w", err)
	}
	return nil
}
func (c *client) start(ctx context.Context, previous *Snapshot) error {
	// Active describes current kernel intent, not whether the server's lease
	// can be requested again. In particular INIT-REBOOT persists an inactive
	// Requesting snapshot; another restart must not forget that lease.
	if previous != nil && previous.Resource == c.config.Resource && previous.Interface == c.config.Interface && previous.HardwareAddress == c.hardware.String() && previous.ClientIdentity == c.config.ClientID && c.now().Before(previous.ExpiresAt) {
		lease := wire.LeaseFromSnapshot(previous.Snapshot)
		if lease.Address.Is4() && !lease.Address.IsUnspecified() && !lease.Address.IsMulticast() && lease.PrefixLength >= 0 && lease.PrefixLength <= 32 && lease.ServerID.Is4() && !lease.ServerID.IsUnspecified() && !lease.ServerID.IsMulticast() && lease.LeaseTime > 0 && !lease.AcquiredAt.After(c.now()) && c.now().Before(lease.ExpiresAt()) {
			c.lease = lease
			c.reboot = true
			return c.begin(ctx, wire.StateRequesting)
		}
	}
	c.lease = wire.Lease{}
	return c.begin(ctx, wire.StateDiscovering)
}
func (c *client) begin(ctx context.Context, state wire.State) error {
	c.state = state
	c.transaction = c.xid()
	c.retry = 4 * time.Second
	c.phaseStarted = c.now()
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
	payload := wire.EncodeRequest(typ, c.transaction, c.hardware, opts)
	for len(payload) < 300 {
		payload = append(payload, 0)
	}
	if c.state == wire.StateRenewing || c.state == wire.StateRebinding {
		copy(payload[12:16], c.lease.Address.AsSlice())
		if c.state == wire.StateRenewing {
			binary.BigEndian.PutUint16(payload[10:12], 0)
		}
	}
	seconds := int(c.now().Sub(c.phaseStarted) / time.Second)
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
	c.nextRetry = c.now().Add(c.retry + variation)
	if c.retry < 64*time.Second {
		c.retry *= 2
	}
	return nil
}
func (c *client) tick(ctx context.Context) error {
	now := c.now()
	if c.lease.Address.IsValid() && !now.Before(c.lease.ExpiresAt()) {
		c.state = wire.StateExpired
		c.lease = wire.Lease{}
		c.reboot = false
		if err := c.publish(ctx); err != nil {
			return err
		}
		return c.begin(ctx, wire.StateDiscovering)
	}
	if (c.state == wire.StateBound || c.state == wire.StateRenewing) && !now.Before(c.lease.RebindAt()) {
		return c.begin(ctx, wire.StateRebinding)
	}
	if c.state == wire.StateBound && !now.Before(c.lease.RenewAt()) {
		return c.begin(ctx, wire.StateRenewing)
	}
	if c.state == wire.StateBound {
		return nil
	}
	if c.state == wire.StateRequesting && now.Sub(c.phaseStarted) >= 60*time.Second {
		c.reboot = false
		c.lease = wire.Lease{}
		return c.begin(ctx, wire.StateDiscovering)
	}
	if !now.Before(c.nextRetry) {
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
	for code, size := range map[byte]int{wire.OptionMessageType: 1, wire.OptionServerID: 4, wire.OptionSubnetMask: 4, wire.OptionLeaseTime: 4, wire.OptionT1: 4, wire.OptionT2: 4, wire.OptionMTU: 2, wire.OptionBroadcastAddress: 4} {
		if b, exists := m.Options[code]; exists && len(b) != size {
			return wire.Message{}, fmt.Errorf("invalid DHCPv4 option %d length", code)
		}
	}
	return m, nil
}
func (c *client) handle(ctx context.Context, payload []byte) error {
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
		c.phaseStarted = c.now()
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
		c.lease = wire.Lease{}
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
		for _, code := range []byte{wire.OptionRouter, wire.OptionDNSServer, wire.OptionNTPServer} {
			if len(m.Options[code])%4 != 0 {
				return nil
			}
		}
		lease := wire.LeaseFromACK(m, c.now().UTC())
		if lease.T1 <= 0 || lease.T2 <= lease.T1 || lease.T2 >= lease.LeaseTime {
			return nil
		}
		c.lease = lease
		c.state = wire.StateBound
		c.reboot = false
		return c.publish(ctx)
	}
	return nil
}
