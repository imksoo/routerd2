// SPDX-License-Identifier: BSD-3-Clause

// Package raclient receives WAN router advertisements and publishes bounded,
// expiring protocol facts. Only the network writer may install those facts.
package raclient

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

const (
	MaxAddresses     = 128
	MaxPrefixes      = 128
	MaxRoutes        = 256
	MaxDNSServers    = 32
	MaxSearchDomains = 32
)

// Config is immutable during a worker lifetime. StatePath is supplied by the
// private runtime compiler, not accepted as remote protocol intent. Nil feature
// flags enable their feature. DNS publishes learned information only; it never
// rewrites resolv.conf. InterfaceID is an IPv6 suffix with its upper 64 bits zero
// (for example ::1234); empty uses the Ethernet modified EUI-64 identity.
type Config struct {
	Resource         string   `json:"resource"`
	Interface        string   `json:"interface"`
	StatePath        string   `json:"state_path"`
	SourceAddress    string   `json:"source_address,omitempty"`
	InterfaceID      string   `json:"interface_id,omitempty"`
	SLAAC            *bool    `json:"slaac,omitempty"`
	DefaultRoute     *bool    `json:"default_route,omitempty"`
	OnLink           *bool    `json:"on_link,omitempty"`
	RouteInformation *bool    `json:"route_information,omitempty"`
	DNS              *bool    `json:"dns,omitempty"`
	AllowedRouters   []string `json:"allowed_routers,omitempty"`
}

type Address struct {
	// A /128 intentionally avoids the kernel's implicit connected /64 route.
	// The L-bit on-link prefix is represented separately in Prefixes.
	Address        string            `json:"address"`
	Prefix         string            `json:"prefix"`
	PreferredUntil lifetime.Deadline `json:"preferred_until"`
	ValidUntil     lifetime.Deadline `json:"valid_until"`
	Deprecated     bool              `json:"deprecated,omitempty"`
	DADFailed      bool              `json:"dad_failed,omitempty"`
}

type Prefix struct {
	Prefix     string            `json:"prefix"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	// Preference is -1 (low), 0 (medium), or 1 (high).
	Preference int               `json:"preference"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type DNSServer struct {
	Address string `json:"address"`
	// Router records the last advertiser, not an independent ownership key.
	// RFC 8106 updates a server's lifetime per receiving interface/address.
	Router     string            `json:"router"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type SearchDomain struct {
	Domain     string            `json:"domain"`
	Router     string            `json:"router"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

// Learned lifetimes retain their original boot scope, including explicit
// infinity. Display timestamps never define protocol authority. Active means
// the receiver is running; it does not assert an address or router was learned.
type Snapshot struct {
	BootID                 string         `json:"boot_id"`
	ObservedNanos          uint64         `json:"observed_nanos"`
	ReceivedNanos          uint64         `json:"received_nanos,omitempty"`
	Resource               string         `json:"resource"`
	Interface              string         `json:"interface"`
	SourceAddress          string         `json:"source_address"`
	InterfaceID            string         `json:"interface_id"`
	Active                 bool           `json:"active"`
	UpdatedAt              time.Time      `json:"updated_at"`
	LastReceivedAt         time.Time      `json:"last_received_at,omitempty"`
	LastRouter             string         `json:"last_router,omitempty"`
	Received               uint64         `json:"received"`
	Rejected               uint64         `json:"rejected"`
	Solicitations          uint64         `json:"solicitations"`
	LastError              string         `json:"last_error,omitempty"`
	Managed                bool           `json:"managed,omitempty"`
	OtherConfig            bool           `json:"other_config,omitempty"`
	MTU                    int            `json:"mtu,omitempty"`
	CurrentHopLimit        int            `json:"current_hop_limit,omitempty"`
	ReachableMilliseconds  uint32         `json:"reachable_milliseconds,omitempty"`
	RetransmitMilliseconds uint32         `json:"retransmit_milliseconds,omitempty"`
	Addresses              []Address      `json:"addresses,omitempty"`
	Prefixes               []Prefix       `json:"prefixes,omitempty"`
	Routes                 []Route        `json:"routes,omitempty"`
	DNSServers             []DNSServer    `json:"dns_servers,omitempty"`
	SearchDomains          []SearchDomain `json:"search_domains,omitempty"`
}

type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

func enabled(v *bool) bool { return v == nil || *v }

func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" || len(c.Resource) > 128 || strings.ContainsAny(c.Resource, "\x00\r\n") {
		return errors.New("RA client resource is required and must be bounded")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") || c.Interface == "." || c.Interface == ".." {
		return errors.New("RA client interface is invalid")
	}
	if !filepath.IsAbs(c.StatePath) || strings.ContainsRune(c.StatePath, '\x00') {
		return errors.New("RA client state_path must be absolute")
	}
	if c.SourceAddress != "" && !linkLocal(c.SourceAddress) {
		return errors.New("RA client source_address must be an unzoned IPv6 link-local address")
	}
	if c.InterfaceID != "" {
		if _, err := interfaceID(c.InterfaceID); err != nil {
			return err
		}
	}
	if len(c.AllowedRouters) > 32 {
		return errors.New("at most 32 allowed RA routers are supported")
	}
	seen := map[string]bool{}
	for _, router := range c.AllowedRouters {
		if !linkLocal(router) {
			return errors.New("allowed RA routers must be unzoned IPv6 link-local addresses")
		}
		a, _ := netip.ParseAddr(router)
		if seen[a.String()] {
			return errors.New("duplicate allowed RA router")
		}
		seen[a.String()] = true
	}
	return nil
}

func linkLocal(value string) bool {
	a, err := netip.ParseAddr(value)
	return err == nil && a.Is6() && a.IsLinkLocalUnicast() && a.Zone() == ""
}

func interfaceID(value string) ([8]byte, error) {
	var out [8]byte
	a, err := netip.ParseAddr(value)
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
		return out, errors.New("RA interface_id must be a nonzero IPv6 suffix with its upper 64 bits zero")
	}
	raw := a.As16()
	for _, b := range raw[:8] {
		if b != 0 {
			return out, errors.New("RA interface_id upper 64 bits must be zero")
		}
	}
	copy(out[:], raw[8:])
	if out == [8]byte{} {
		return out, errors.New("RA interface_id cannot be the subnet-router anycast identifier zero")
	}
	return out, nil
}

func suffixString(iid [8]byte) string {
	var raw [16]byte
	copy(raw[8:], iid[:])
	return netip.AddrFrom16(raw).String()
}

func allowed(c Config, router string) bool {
	if !linkLocal(router) {
		return false
	}
	if len(c.AllowedRouters) == 0 {
		return true
	}
	a, _ := netip.ParseAddr(router)
	for _, value := range c.AllowedRouters {
		b, _ := netip.ParseAddr(value)
		if a == b {
			return true
		}
	}
	return false
}

// ValidateSnapshot is pure and suitable at the single-writer IPC boundary. It
// checks identities, feature scope, address derivation, uniqueness and limits;
// a producer cannot smuggle an arbitrary address or a non-link-local gateway.
func ValidateSnapshot(c Config, s Snapshot) error {
	if err := Validate(c); err != nil {
		return err
	}
	if s.Resource != c.Resource || s.Interface != c.Interface || !canonicalLinkLocal(s.SourceAddress) {
		return errors.New("RA snapshot resource/interface/source identity mismatch")
	}
	if c.SourceAddress != "" {
		a, _ := netip.ParseAddr(c.SourceAddress)
		b, _ := netip.ParseAddr(s.SourceAddress)
		if a != b {
			return errors.New("RA snapshot source_address differs from configured identity")
		}
	}
	iid, err := interfaceID(s.InterfaceID)
	if err != nil {
		return err
	}
	if s.InterfaceID != suffixString(iid) {
		return errors.New("RA snapshot interface_id must be canonical")
	}
	if c.InterfaceID != "" {
		want, _ := interfaceID(c.InterfaceID)
		if want != iid {
			return errors.New("RA snapshot interface_id differs from configured identity")
		}
	}
	if len(s.Addresses) > MaxAddresses || len(s.Prefixes) > MaxPrefixes || len(s.Routes) > MaxRoutes || len(s.DNSServers) > MaxDNSServers || len(s.SearchDomains) > MaxSearchDomains {
		return errors.New("RA snapshot exceeds a bounded collection limit")
	}
	if !enabled(c.SLAAC) && len(s.Addresses) > 0 || !enabled(c.OnLink) && len(s.Prefixes) > 0 || !enabled(c.DNS) && len(s.DNSServers)+len(s.SearchDomains) > 0 {
		return errors.New("RA snapshot contains a disabled feature")
	}
	if s.UpdatedAt.IsZero() || s.UpdatedAt.Year() < 1970 || s.UpdatedAt.Year() > 9999 || len(s.LastError) > 512 || s.MTU != 0 && (s.MTU < 1280 || s.MTU > 65535) || s.CurrentHopLimit < 0 || s.CurrentHopLimit > 255 || s.ReachableMilliseconds > 3600000 || !s.LastReceivedAt.IsZero() && (s.LastReceivedAt.Year() < 1970 || s.LastReceivedAt.Year() > 9999) {
		return errors.New("RA snapshot contains invalid status metadata")
	}
	if !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ObservedNanos > uint64(1<<63-1) || s.ReceivedNanos > s.ObservedNanos {
		return errors.New("RA snapshot contains invalid boot metadata")
	}
	deadline := func(d lifetime.Deadline) bool {
		return d.Validate() == nil && d.UTC.IsZero() && d.BootID == s.BootID && (d.Nanos == 0 || d.Nanos <= s.ObservedNanos+uint64(time.Duration(^uint32(0))*time.Second))
	}
	if s.LastRouter != "" && (!canonicalLinkLocal(s.LastRouter) || !allowed(c, s.LastRouter)) {
		return errors.New("RA snapshot last router is outside configured scope")
	}
	seen := map[string]bool{}
	for _, a := range s.Addresses {
		p, err := netip.ParsePrefix(a.Prefix)
		v, e := netip.ParsePrefix(a.Address)
		if err != nil || e != nil || p.Bits() != 64 || p != p.Masked() || p.String() != a.Prefix || !validPrefix(p) || v.Bits() != 128 || v.String() != a.Address || !v.Addr().Is6() || !p.Contains(v.Addr()) || seen[a.Address] {
			return errors.New("RA snapshot contains an invalid or duplicate SLAAC address")
		}
		raw := p.Addr().As16()
		copy(raw[8:], iid[:])
		if v.Addr() != netip.AddrFrom16(raw) {
			return errors.New("RA snapshot address does not derive from the configured interface ID")
		}
		if !deadline(a.ValidUntil) || !deadline(a.PreferredUntil) || !lifetime.Ordered(a.PreferredUntil, a.ValidUntil) {
			return errors.New("RA snapshot address lifetimes are invalid")
		}
		seen[a.Address] = true
	}
	seen = map[string]bool{}
	for _, p := range s.Prefixes {
		n, err := netip.ParsePrefix(p.Prefix)
		if err != nil || n != n.Masked() || n.String() != p.Prefix || !validPrefix(n) || seen[p.Prefix] || !deadline(p.ValidUntil) {
			return errors.New("RA snapshot contains an invalid on-link prefix")
		}
		seen[p.Prefix] = true
	}
	seen = map[string]bool{}
	for _, r := range s.Routes {
		p, err := netip.ParsePrefix(r.Destination)
		key := r.Destination + "|" + r.Gateway
		if err != nil || p != p.Masked() || p.String() != r.Destination || !validPrefix(p) || !canonicalLinkLocal(r.Gateway) || !allowed(c, r.Gateway) || r.Preference < -1 || r.Preference > 1 || seen[key] || !deadline(r.ValidUntil) {
			return errors.New("RA snapshot contains an invalid route")
		}
		if p.Bits() == 0 && !enabled(c.DefaultRoute) || p.Bits() != 0 && !enabled(c.RouteInformation) {
			return errors.New("RA snapshot route is outside configured feature scope")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, d := range s.DNSServers {
		a, err := netip.ParseAddr(d.Address)
		if err != nil || !a.Is6() || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() || a.Zone() != "" || a.String() != d.Address || seen[d.Address] || !canonicalLinkLocal(d.Router) || !allowed(c, d.Router) || !deadline(d.ValidUntil) {
			return errors.New("RA snapshot contains an invalid DNS server")
		}
		seen[d.Address] = true
	}
	seen = map[string]bool{}
	for _, d := range s.SearchDomains {
		if !validDomain(d.Domain) || seen[d.Domain] || !canonicalLinkLocal(d.Router) || !allowed(c, d.Router) || !deadline(d.ValidUntil) {
			return errors.New("RA snapshot contains an invalid DNS search domain")
		}
		seen[d.Domain] = true
	}
	return nil
}

func canonicalLinkLocal(value string) bool {
	a, err := netip.ParseAddr(value)
	return err == nil && a.Is6() && a.IsLinkLocalUnicast() && a.Zone() == "" && a.String() == value
}

func validPrefix(p netip.Prefix) bool {
	a := p.Addr()
	return p.IsValid() && a.Is6() && !a.Is4In6() && !a.IsMulticast() && !a.IsLinkLocalUnicast() && !a.IsLoopback() && (!a.IsUnspecified() || p.Bits() == 0)
}

func validDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 || domain != strings.ToLower(domain) {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func snapshotError(kind string) error { return fmt.Errorf("invalid router advertisement: %s", kind) }
