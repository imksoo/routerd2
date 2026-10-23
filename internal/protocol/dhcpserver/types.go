// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

type Config = dhcpserverconfig.Config
type Scope = dhcpserverconfig.Scope
type Reservation = dhcpserverconfig.Reservation
type Lease = dhcpserverconfig.Lease
type Snapshot = dhcpserverconfig.Snapshot
type Stats = dhcpserverconfig.Stats
type Event = dhcpserverconfig.Event

func Normalize(c Config) Config { return dhcpserverconfig.Normalize(c) }
func Validate(c Config) error   { return dhcpserverconfig.Validate(c) }

type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

type packet struct {
	Data           []byte
	Source         netip.Addr
	SourcePort     int
	Destination    netip.Addr
	InterfaceIndex int
}
type response struct {
	Data        []byte
	Destination netip.Addr
	Port        int
	MAC         net.HardwareAddr
	// Relay and renew responses use UDP routing. Initial direct replies use
	// exact Ethernet destination, so a client need not answer ARP before ACK.
	Routed bool
}
type transport interface {
	Receive(context.Context) (packet, error)
	Send(context.Context, response) error
	Probe(context.Context, netip.Addr, net.HardwareAddr) (bool, error)
	Check() error
	Close() error
}

type storedLease struct {
	Lease
	HeldUntil time.Time `json:"held_until"`
	Expired   bool      `json:"expired,omitempty"`
}
type quarantine struct {
	IPAddress string    `json:"ip_address"`
	Until     time.Time `json:"until"`
}
type offer struct {
	Lease  Lease
	XID    uint32
	Until  time.Time
	Probed bool
}
