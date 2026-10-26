// SPDX-License-Identifier: BSD-3-Clause

// Package bfd implements asynchronous single-hop and multihop BFD sessions. It owns
// sockets and observations only; routing decisions remain with its consumer.
package bfd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

const (
	StateAdminDown = "ADMIN_DOWN"
	StateDown      = "DOWN"
	StateInit      = "INIT"
	StateUp        = "UP"
	MaxPeers       = 64
	singleHopPort  = 3784
	multihopPort   = 4784
)

type Config struct {
	Resource                  string `json:"resource"`
	Interface                 string `json:"interface,omitempty"`
	VRF                       string `json:"vrf,omitempty"`
	SourceAddress             string `json:"source_address"`
	Peers                     []Peer `json:"peers"`
	PointToPoint              bool   `json:"point_to_point,omitempty"`
	Multihop                  bool   `json:"multihop,omitempty"`
	EffectTimeoutMilliseconds int    `json:"effect_timeout_milliseconds,omitempty"`
	// Unsupported modes have explicit rejection instead of implicit fallback.
	Authentication string `json:"authentication,omitempty"`
	DemandMode     bool   `json:"demand_mode,omitempty"`
	Echo           bool   `json:"echo,omitempty"`
	Passive        bool   `json:"passive,omitempty"`
}

type Peer struct {
	Address           string `json:"address"`
	MinTxMilliseconds int    `json:"min_tx_milliseconds,omitempty"`
	MinRxMilliseconds int    `json:"min_rx_milliseconds,omitempty"`
	DetectMultiplier  int    `json:"detect_multiplier,omitempty"`
}

// Clock relates an absolute CLOCK_BOOTTIME value to display-only UTC. Neither
// peer health nor expiry may rely only on wall time. BootID scopes monotonic
// deadlines to the current kernel, including when userland restarts.
type Clock = boottime.Clock

type Snapshot struct {
	Resource         string      `json:"resource"`
	Interface        string      `json:"interface"`
	SourceAddress    string      `json:"source_address"`
	BootID           string      `json:"boot_id"`
	Active           bool        `json:"active"`
	ObservedNanos    uint64      `json:"observed_nanos"`
	ExpiresNanos     uint64      `json:"expires_nanos"`
	TimestampChanges uint64      `json:"timestamp_changes"`
	UpdatedAt        time.Time   `json:"updated_at"`
	Peers            []PeerState `json:"peers"`
	Rejected         uint64      `json:"rejected"`
	ReceiveDropped   uint64      `json:"receive_dropped"`
	LastError        string      `json:"last_error,omitempty"`
}

type PeerState struct {
	Address                      string    `json:"address"`
	ObservedSource               string    `json:"observed_source,omitempty"`
	State                        string    `json:"state"`
	RemoteState                  string    `json:"remote_state"`
	Diagnostic                   uint8     `json:"diagnostic"`
	RemoteDiagnostic             uint8     `json:"remote_diagnostic"`
	LocalDiscriminator           uint32    `json:"local_discriminator"`
	RemoteDiscriminator          uint32    `json:"remote_discriminator"`
	SourcePort                   uint16    `json:"source_port"`
	DesiredMinTxMicroseconds     uint32    `json:"desired_min_tx_microseconds"`
	RequiredMinRxMicroseconds    uint32    `json:"required_min_rx_microseconds"`
	RemoteMinTxMicroseconds      uint32    `json:"remote_min_tx_microseconds"`
	RemoteMinRxMicroseconds      uint32    `json:"remote_min_rx_microseconds"`
	RemoteMinEchoRxMicroseconds  uint32    `json:"remote_min_echo_rx_microseconds"`
	RemoteDetectMultiplier       uint8     `json:"remote_detect_multiplier"`
	TransmitIntervalMicroseconds uint32    `json:"transmit_interval_microseconds"`
	DetectionTimeMicroseconds    uint64    `json:"detection_time_microseconds"`
	Polling                      bool      `json:"polling"`
	LastReceivedAt               time.Time `json:"last_received_at,omitzero"`
	LastReceivedBootNanoseconds  int64     `json:"last_received_boot_nanoseconds,omitempty"`
	DeadlineBootNanoseconds      int64     `json:"deadline_boot_nanoseconds,omitempty"`
	ValidUntil                   time.Time `json:"valid_until,omitzero"`
	Transitions                  uint64    `json:"transitions"`
	UpTransitions                uint64    `json:"up_transitions"`
	// A coalesced snapshot retains short Up->Down->Up failures. Consumers must
	// compare this counter within the enclosing producer epoch before accepting
	// an Up refresh; counter changes can require resetting a dependent session.
	DownTransitions  uint64 `json:"down_transitions"`
	Received         uint64 `json:"received"`
	Transmitted      uint64 `json:"transmitted"`
	TransmitFailures uint64 `json:"transmit_failures"`
	Rejected         uint64 `json:"rejected"`
}

type Effects interface {
	// Update is serialized, and must return when its context is cancelled.
	// A slow call coalesces later snapshots without stopping the wire loop.
	// Run joins the last active call before publishing final withdrawal.
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, snapshot Snapshot) error { return f(ctx, snapshot) }

func Normalize(c Config) Config {
	c.Peers = append([]Peer(nil), c.Peers...)
	if c.EffectTimeoutMilliseconds == 0 {
		c.EffectTimeoutMilliseconds = 1000
	}
	for i := range c.Peers {
		if c.Peers[i].MinTxMilliseconds == 0 {
			c.Peers[i].MinTxMilliseconds = 300
		}
		if c.Peers[i].MinRxMilliseconds == 0 {
			c.Peers[i].MinRxMilliseconds = 300
		}
		if c.Peers[i].DetectMultiplier == 0 {
			c.Peers[i].DetectMultiplier = 3
		}
	}
	return c
}

func validInterface(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 15 && !strings.ContainsAny(value, "/\x00 \t\r\n:")
}
func unicast(value string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(value)
	return a, err == nil && a.String() == value && a.Zone() == "" && !a.Is4In6() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLoopback() && a != netip.MustParseAddr("255.255.255.255")
}

func Validate(c Config) error {
	c = Normalize(c)
	if strings.TrimSpace(c.Resource) == "" || len(c.Resource) > 128 || strings.ContainsAny(c.Resource, "\x00\r\n") {
		return errors.New("BFD resource must be nonempty and bounded")
	}
	if c.Multihop && (c.Interface != "" || c.PointToPoint) {
		return errors.New("multihop BFD uses routed address pairs without an interface or point_to_point mode")
	}
	if !c.Multihop && !validInterface(c.Interface) {
		return errors.New("BFD requires an explicit valid interface")
	}
	if c.VRF != "" && (!validInterface(c.VRF) || c.VRF == c.Interface) {
		return errors.New("BFD vrf must name a valid routing master distinct from its single-hop interface")
	}
	if c.Authentication != "" && c.Authentication != "none" || c.DemandMode || c.Echo || c.Passive {
		return errors.New("BFD supports active asynchronous control only; authentication, demand, echo and passive modes are unavailable")
	}
	source, ok := unicast(c.SourceAddress)
	if !ok || c.Multihop && source.IsLinkLocalUnicast() {
		return errors.New("BFD requires an explicit canonical local unicast address without a zone")
	}
	if len(c.Peers) == 0 || len(c.Peers) > MaxPeers || c.PointToPoint && len(c.Peers) != 1 {
		return errors.New("BFD requires 1..64 peers, or exactly one for point-to-point mode")
	}
	if c.EffectTimeoutMilliseconds < 10 || c.EffectTimeoutMilliseconds > 5000 {
		return errors.New("BFD effect timeout must be 10..5000 milliseconds")
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		a, ok := unicast(p.Address)
		if !ok || a.Is4() != source.Is4() || a == source || seen[p.Address] || c.Multihop && a.IsLinkLocalUnicast() {
			return errors.New("BFD peers must be distinct canonical unicast addresses of the source family")
		}
		seen[p.Address] = true
		if p.MinTxMilliseconds < 50 || p.MinTxMilliseconds > 60000 || p.MinRxMilliseconds < 50 || p.MinRxMilliseconds > 60000 || p.DetectMultiplier < 1 || p.DetectMultiplier > 255 {
			return errors.New("BFD intervals must be 50..60000 milliseconds and multiplier 1..255")
		}
	}
	return nil
}

func validState(value string) bool {
	return value == StateAdminDown || value == StateDown || value == StateInit || value == StateUp
}
func validBootID(value string) bool { return boottime.ValidBootID(value) }

func validWall(t time.Time) bool { return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 }

func ValidateSnapshot(c Config, s Snapshot) error {
	c = Normalize(c)
	if err := Validate(c); err != nil {
		return err
	}
	if s.TimestampChanges == 0 || s.ObservedNanos == 0 || s.ObservedNanos > math.MaxInt64 || s.ExpiresNanos != s.ObservedNanos+uint64(StatusLifetime) || s.Resource != c.Resource || s.Interface != c.Interface || s.SourceAddress != c.SourceAddress || !validBootID(s.BootID) || !validWall(s.UpdatedAt) || len(s.Peers) != len(c.Peers) || len(s.LastError) > 256 {
		return errors.New("BFD snapshot scope or bounded metadata is invalid")
	}
	seenDiscriminators := map[uint32]bool{}
	for i, p := range s.Peers {
		cfg := c.Peers[i]
		balanced := p.UpTransitions == p.DownTransitions
		if p.State == StateUp && p.DownTransitions != math.MaxUint64 {
			balanced = p.UpTransitions == p.DownTransitions+1
		}
		// Both counters saturate. Once both reached MaxUint64 an additional
		// Up transition cannot be represented, but every unsaturated state must
		// retain the exact outstanding Up/down balance.
		if p.LastReceivedBootNanoseconds > int64(s.ObservedNanos) || p.Address != cfg.Address || !validState(p.State) || !validState(p.RemoteState) || p.Diagnostic > 31 || p.RemoteDiagnostic > 31 || p.LocalDiscriminator == 0 || seenDiscriminators[p.LocalDiscriminator] || p.SourcePort < 49152 || p.RequiredMinRxMicroseconds != uint32(cfg.MinRxMilliseconds)*1000 || p.UpTransitions > p.Transitions || p.DownTransitions > p.UpTransitions || !balanced {
			return errors.New("BFD peer snapshot identity, state or counters are invalid")
		}
		seenDiscriminators[p.LocalDiscriminator] = true
		wantTx := uint32(cfg.MinTxMilliseconds) * 1000
		if p.State != StateUp {
			wantTx = max(wantTx, 1_000_000)
		}
		if p.DesiredMinTxMicroseconds != wantTx || p.TransmitIntervalMicroseconds != max(wantTx, p.RemoteMinRxMicroseconds) {
			return errors.New("BFD snapshot transmit interval differs from negotiated intent")
		}
		if p.ObservedSource != "" {
			source, ok := unicast(p.ObservedSource)
			local, _ := netip.ParseAddr(c.SourceAddress)
			if !ok || source.Is4() != local.Is4() || source == local || !c.PointToPoint && p.ObservedSource != p.Address {
				return errors.New("BFD snapshot observed source is outside session scope")
			}
		}
		if p.Received == 0 {
			if p.RemoteDiscriminator != 0 || !p.LastReceivedAt.IsZero() || p.LastReceivedBootNanoseconds != 0 || p.ObservedSource != "" || p.RemoteDetectMultiplier != 0 {
				return errors.New("BFD unreceived peer contains learned facts")
			}
		} else if !validWall(p.LastReceivedAt) || p.LastReceivedBootNanoseconds <= 0 || p.RemoteDetectMultiplier == 0 || p.RemoteMinTxMicroseconds == 0 || p.ObservedSource == "" {
			return errors.New("BFD received peer lacks complete remote timing identity")
		}
		detect := uint64(p.RemoteDetectMultiplier) * uint64(max(p.RequiredMinRxMicroseconds, p.RemoteMinTxMicroseconds))
		if p.DetectionTimeMicroseconds != detect {
			return errors.New("BFD snapshot detection interval is invalid")
		}
		if s.Active && (p.State == StateInit || p.State == StateUp) {
			if p.Received == 0 || p.RemoteDiscriminator == 0 || p.RemoteState == StateAdminDown || p.State == StateUp && (p.UpTransitions == 0 || p.RemoteState == StateDown) || p.State == StateInit && p.RemoteState != StateDown || p.DeadlineBootNanoseconds <= p.LastReceivedBootNanoseconds || uint64(p.DeadlineBootNanoseconds-p.LastReceivedBootNanoseconds) != detect*1000 || !p.ValidUntil.Equal(p.LastReceivedAt.Add(time.Duration(detect)*time.Microsecond)) {
				return errors.New("BFD live session lacks a receive-anchored finite detection deadline")
			}
		} else if p.DeadlineBootNanoseconds != 0 || !p.ValidUntil.IsZero() || !s.Active && p.State != StateAdminDown {
			return errors.New("BFD inactive/down snapshot contains a health grant")
		}
	}
	return nil
}

// Healthy validates the complete snapshot and checks one exact configured peer
// against the current kernel clock. Receipt/replay of an old snapshot never
// renews this grant. The caller must also fence its enclosing producer epoch,
// config digest and sequence and observe DownTransitions before gating traffic.
func Healthy(c Config, s Snapshot, address string, now Clock) bool {
	if !s.Active || s.BootID != now.BootID || now.BoottimeNanoseconds <= 0 || ValidateSnapshot(c, s) != nil {
		return false
	}
	for _, p := range s.Peers {
		if p.Address == address {
			return p.State == StateUp && p.LastReceivedBootNanoseconds <= now.BoottimeNanoseconds && now.BoottimeNanoseconds < p.DeadlineBootNanoseconds
		}
	}
	return false
}

func describePeer(index int, err error) error { return fmt.Errorf("BFD peer %d: %w", index, err) }
