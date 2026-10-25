// Package vrrp implements independent IPv4 and IPv6 VRRPv3 elections.
// The wire protocol follows RFC 9568. Kernel ownership is delegated to an
// acknowledged Effects implementation; this package never adds or deletes a
// VIP, changes a link, or invokes keepalived, ip, nft, or a service manager.
package vrrp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/factcondition"
)

const (
	StateInitialize = "INITIALIZE"
	StateBackup     = "BACKUP"
	StateActive     = "ACTIVE"
)

type GracefulActivation struct {
	ReadyWhen factcondition.Expression `json:"ready_when"`
	Timeout   string                   `json:"timeout,omitempty"`
}

type Config struct {
	Version                    int                 `json:"version,omitempty"`
	AuthPassword               string              `json:"auth_password,omitempty"`
	Track                      []Track             `json:"track,omitempty"`
	GracefulActivation         *GracefulActivation `json:"graceful_activation,omitempty"`
	Resource                   string              `json:"resource"`
	Interface                  string              `json:"interface"`
	VIPInterface               string              `json:"vip_interface,omitempty"`
	VRF                        string              `json:"vrf,omitempty"`
	Family                     string              `json:"family"`
	SourceAddress              string              `json:"source_address,omitempty"`
	UnicastPeers               []netip.Addr        `json:"unicast_peers,omitempty"`
	VirtualRouterID            int                 `json:"virtual_router_id"`
	Priority                   int                 `json:"priority,omitempty"`
	AdvertIntervalCentiseconds int                 `json:"advert_interval_centiseconds,omitempty"`
	EffectTimeoutMilliseconds  int                 `json:"effect_timeout_milliseconds,omitempty"`
	Addresses                  []string            `json:"addresses"`
	Preempt                    *bool               `json:"preempt,omitempty"`
	AddressOwner               bool                `json:"address_owner,omitempty"`
	AcceptMode                 bool                `json:"accept_mode,omitempty"`
}

// Snapshot reports election state and the worker's requested publication state.
// The coordinator decides admission, applies the VIP and returns its actual
// ownership from Effects.Apply. Election messages cannot grant themselves a VIP.
type Snapshot struct {
	Version                int           `json:"version"`
	Track                  []TrackStatus `json:"track,omitempty"`
	ActivationState        string        `json:"activation_state,omitempty"`
	ActivationStartedNanos uint64        `json:"activation_started_nanos,omitempty"`
	BootID                 string        `json:"boot_id"`
	ObservedNanos          uint64        `json:"observed_nanos"`
	ExpiresNanos           uint64        `json:"expires_nanos"`
	Resource               string        `json:"resource"`
	Interface              string        `json:"interface"`
	VIPInterface           string        `json:"vip_interface"`
	VRF                    string        `json:"vrf,omitempty"`
	Family                 string        `json:"family"`
	SourceAddress          string        `json:"source_address"`
	VirtualRouterID        int           `json:"virtual_router_id"`
	VirtualMAC             string        `json:"virtual_mac"`
	Addresses              []string      `json:"addresses"`
	Active                 bool          `json:"active"`
	State                  string        `json:"state"`
	AcceptMode             bool          `json:"accept_mode"`
	AddressOwner           bool          `json:"address_owner"`
	Epoch                  string        `json:"epoch"`
	Transition             uint64        `json:"transition"`
	Priority               int           `json:"priority"`
	Peer                   string        `json:"peer,omitempty"`
	PeerPriority           int           `json:"peer_priority,omitempty"`
	ActiveIntervalCS       int           `json:"active_interval_centiseconds"`
	Deadline               time.Time     `json:"deadline,omitempty"`
	// Display metadata only. ExpiresNanos in BootID defines actual authority.
	ValidUntil       time.Time `json:"valid_until,omitempty"`
	Advertisements   uint64    `json:"advertisements"`
	Received         uint64    `json:"received"`
	Rejected         uint64    `json:"rejected"`
	ReceiveDropped   uint64    `json:"receive_dropped"`
	IntervalMismatch uint64    `json:"interval_mismatch"`
	LastError        string    `json:"last_error,omitempty"`
}

// Effects applies one election observation and returns ownership and tracking.
type Effects interface {
	Apply(context.Context, Snapshot) (Decision, error)
}

type EffectsFunc func(context.Context, Snapshot) (Decision, error)

func (f EffectsFunc) Apply(ctx context.Context, s Snapshot) (Decision, error) { return f(ctx, s) }

func Normalize(c Config) Config {
	if c.Version == 0 {
		c.Version = 3
	}
	if c.Track != nil {
		c.Track = append([]Track(nil), c.Track...)
		for i := range c.Track {
			if c.Track[i].Penalty == 0 {
				c.Track[i].Penalty = 50
			}
			if c.Track[i].HealthyThreshold == 0 {
				c.Track[i].HealthyThreshold = 2
			}
			if c.Track[i].UnhealthyThreshold == 0 {
				c.Track[i].UnhealthyThreshold = 3
			}
		}
	}
	if c.GracefulActivation != nil {
		copy := *c.GracefulActivation
		if copy.Timeout == "" {
			copy.Timeout = "45s"
		}
		c.GracefulActivation = &copy
	}
	if a, err := netip.ParseAddr(c.SourceAddress); err == nil {
		c.SourceAddress = a.String()
	}
	if c.Priority == 0 {
		c.Priority = 100
		if c.AddressOwner {
			c.Priority = 255
		}
	}
	if c.AdvertIntervalCentiseconds == 0 {
		c.AdvertIntervalCentiseconds = 100
	}
	if c.EffectTimeoutMilliseconds == 0 {
		c.EffectTimeoutMilliseconds = 2000
	}
	if c.VIPInterface == "" {
		sum := sha256.Sum256([]byte(c.Resource + "\x00" + c.Interface + "\x00" + c.Family))
		family := 4
		if c.Family == "ipv6" {
			family = 6
		}
		c.VIPInterface = fmt.Sprintf("vr%d%02x%x", family, c.VirtualRouterID, sum[:4])
	}
	return c
}

func validInterface(s string) bool {
	return s != "" && len(s) <= 15 && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00 \t\r\n:")
}

func Validate(c Config) error {
	c = Normalize(c)
	if len(c.Track) > 16 || len(c.Track) > 0 && c.AddressOwner {
		return errors.New("VRRP track supports at most 16 sources and cannot change an address owner's priority 255")
	}
	tracked := make(map[string]bool, len(c.Track))
	for _, track := range c.Track {
		if track.Service == "" || strings.TrimSpace(track.Service) != track.Service || len(track.Service) > 24 || tracked[track.Service] || track.Penalty < 1 || track.Penalty > 254 || track.HealthyThreshold < 1 || track.HealthyThreshold > 255 || track.UnhealthyThreshold < 1 || track.UnhealthyThreshold > 255 || track.MaxAgeMillis < 1 || track.MaxAgeMillis > 86400000 {
			return errors.New("VRRP track requires distinct Health services, penalty 1..254, thresholds 1..255 and max_age_millis 1..86400000")
		}
		tracked[track.Service] = true
	}
	if c.GracefulActivation != nil {
		if err := factcondition.Validate(c.GracefulActivation.ReadyWhen); err != nil {
			return fmt.Errorf("VRRP graceful_activation.ready_when: %w", err)
		}
		if timeout, err := time.ParseDuration(c.GracefulActivation.Timeout); err != nil || timeout <= 0 {
			return errors.New("VRRP graceful_activation.timeout requires a positive finite duration")
		}
	}
	if strings.TrimSpace(c.Resource) == "" || len(c.Resource) > 256 || strings.ContainsAny(c.Resource, "\x00\r\n") {
		return errors.New("VRRP resource must be a nonempty bounded identity")
	}
	if !validInterface(c.Interface) || !validInterface(c.VIPInterface) || c.Interface == c.VIPInterface || c.VRF != "" && !validInterface(c.VRF) {
		return errors.New("VRRP requires valid, distinct election and VIP interfaces")
	}
	if c.Family != "ipv4" && c.Family != "ipv6" {
		return errors.New("VRRP family must be ipv4 or ipv6")
	}
	if c.VirtualRouterID < 1 || c.VirtualRouterID > 255 {
		return errors.New("VRRP virtual_router_id must be 1..255")
	}
	if c.Priority < 1 || c.Priority > 255 || (c.Priority == 255) != c.AddressOwner {
		return errors.New("VRRP priority must be 1..254; 255 is reserved for an explicit address_owner")
	}
	switch c.Version {
	case 2:
		if c.Family != "ipv4" || c.AdvertIntervalCentiseconds < 100 || c.AdvertIntervalCentiseconds > 25500 || c.AdvertIntervalCentiseconds%100 != 0 {
			return errors.New("VRRPv2 requires IPv4 and a whole-second advertisement interval in 1..255 seconds")
		}
	case 3:
		if c.AdvertIntervalCentiseconds < 1 || c.AdvertIntervalCentiseconds > 4095 {
			return errors.New("VRRPv3 advertisement interval must be 1..4095 centiseconds")
		}
	default:
		return errors.New("VRRP version must be 2 or 3")
	}
	if c.AuthPassword != "" {
		if c.Version != 2 || len(c.AuthPassword) > 8 {
			return errors.New("VRRP auth_password requires version 2 and at most 8 printable ASCII bytes")
		}
		for _, b := range []byte(c.AuthPassword) {
			if b < 32 || b > 126 {
				return errors.New("VRRP auth_password must contain printable ASCII bytes")
			}
		}
	}
	if c.EffectTimeoutMilliseconds < 10 || c.EffectTimeoutMilliseconds > 10000 {
		return errors.New("VRRP effect timeout must be 10..10000 milliseconds")
	}
	if len(c.Addresses) == 0 || len(c.Addresses) > 255 {
		return errors.New("VRRP requires 1..255 virtual host CIDRs of one family")
	}
	seen := make(map[netip.Addr]bool, len(c.Addresses))
	for i, value := range c.Addresses {
		p, err := netip.ParsePrefix(value)
		if err != nil || !addressInFamily(p.Addr(), c.Family) {
			return fmt.Errorf("invalid VRRP virtual address %q", value)
		}
		if seen[p.Addr()] {
			return errors.New("duplicate VRRP virtual address")
		}
		seen[p.Addr()] = true
		if c.Family == "ipv6" && i == 0 && !p.Addr().IsLinkLocalUnicast() {
			return errors.New("the first IPv6 VRRP virtual address must be link-local")
		}
	}
	if c.SourceAddress != "" {
		a, err := netip.ParseAddr(c.SourceAddress)
		if err != nil || !addressInFamily(a, c.Family) || len(c.UnicastPeers) == 0 && c.Family == "ipv6" && !a.IsLinkLocalUnicast() {
			return errors.New("VRRP source must be a same-family unicast address; multicast IPv6 requires link-local")
		}
	}
	if len(c.UnicastPeers) > 0 && c.SourceAddress == "" {
		return errors.New("VRRP unicast requires an explicit source_address")
	}
	if c.UnicastPeers != nil && len(c.UnicastPeers) == 0 {
		return errors.New("VRRP unicast_peers must not be empty; omit it to use multicast")
	}
	if len(c.UnicastPeers) > 128 {
		return errors.New("VRRP unicast supports at most 128 peers")
	}
	peers := make(map[netip.Addr]bool, len(c.UnicastPeers))
	for _, peer := range c.UnicastPeers {
		if !addressInFamily(peer, c.Family) || peers[peer] || peer.String() == c.SourceAddress {
			return errors.New("VRRP unicast peers must be distinct same-family unicast addresses different from the source")
		}
		peers[peer] = true
	}
	return nil
}

func addressInFamily(a netip.Addr, family string) bool {
	if !a.IsValid() || a.Zone() != "" || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() {
		return false
	}
	if family == "ipv4" {
		return a.Is4() && a != netip.MustParseAddr("255.255.255.255")
	}
	return family == "ipv6" && a.Is6()
}

func VirtualMAC(family string, vrid int) net.HardwareAddr {
	marker := byte(1)
	if family == "ipv6" {
		marker = 2
	}
	return net.HardwareAddr{0, 0, 0x5e, 0, marker, byte(vrid)}
}

func (c Config) addresses() []netip.Addr {
	out := make([]netip.Addr, 0, len(c.Addresses))
	for _, address := range c.Addresses {
		out = append(out, netip.MustParsePrefix(address).Addr())
	}
	return out
}

func (c Config) interval() time.Duration {
	return time.Duration(c.AdvertIntervalCentiseconds) * 10 * time.Millisecond
}

func (c Config) preempt() bool { return c.Preempt == nil || *c.Preempt }
