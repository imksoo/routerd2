// Package dhcp6serverconfig describes LAN DHCPv6 server intent.
package dhcp6serverconfig

import (
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/miekg/dns"
)

// Pool selects an address range served by the bundled dnsmasq engine.
// Suffix ranges (::100 to ::1ff) follow the interface's advertised /64.
type Pool struct {
	Start        netip.Addr `json:"start"`
	End          netip.Addr `json:"end"`
	PrefixLength uint8      `json:"prefix_length,omitempty"`
	LeaseSeconds uint32     `json:"lease_seconds,omitempty"`
}

type Config struct {
	Pool           *Pool        `json:"pool,omitempty"`
	LogDHCP        bool         `json:"log_dhcp,omitempty"`
	Resource       string       `json:"resource"`
	Interface      string       `json:"interface"`
	StatePath      string       `json:"state_path"`
	ServerDUID     string       `json:"server_duid,omitempty"`
	DNSServers     []netip.Addr `json:"dns_servers,omitempty"`
	SNTPServers    []netip.Addr `json:"sntp_servers,omitempty"`
	DomainSearch   []string     `json:"domain_search,omitempty"`
	RefreshSeconds uint32       `json:"refresh_seconds,omitempty"`
}

// Learned entries retain the producer's original deadline. Static options
// stay in Config and do not disappear when an unrelated learned entry expires.
type Address struct {
	Address    netip.Addr        `json:"address"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}
type Domain struct {
	Name       string            `json:"name"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}
type Input struct {
	// Authority bounds allocation facts; DNS readiness gates serving without shortening
	// the lifetime of an already observed allocation fact.
	Authority     lifetime.Deadline `json:"authority"`
	DNSReadyUntil lifetime.Deadline `json:"dns_ready_until"`
	DNSServers    []Address         `json:"dns_servers,omitempty"`
	SNTPServers   []Address         `json:"sntp_servers,omitempty"`
	DomainSearch  []Domain          `json:"domain_search,omitempty"`
}

// MaxLeases is the bundled dnsmasq engine's configured default.
const MaxLeases = 1000
const ObservationLifetime = 5 * time.Second

type Lease struct {
	Address   netip.Addr `json:"address"`
	Hostname  string     `json:"hostname,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
}

type Snapshot struct {
	Active        bool      `json:"active"`
	ObservedAt    time.Time `json:"observed_at,omitempty"`
	BootID        string    `json:"boot_id,omitempty"`
	ObservedNanos uint64    `json:"observed_nanos,omitempty"`
	ExpiresNanos  uint64    `json:"expires_nanos,omitempty"`
	Leases        []Lease   `json:"leases,omitempty"`
	Resource      string    `json:"resource"`
	Interface     string    `json:"interface"`
	ServerDUID    string    `json:"server_duid"`
}

func Validate(c Config) error {
	if c.Resource == "" || c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") || !filepath.IsAbs(c.StatePath) {
		return errors.New("DHCPv6 server requires a resource, interface and private state path")
	}
	if c.Pool != nil {
		bits := c.Pool.PrefixLength
		if bits == 0 {
			bits = 64
		}
		if bits < 64 || bits > 128 {
			return errors.New("DHCPv6 pool prefix length must be between 64 and 128")
		}
		if !UsableAddress(c.Pool.Start) || !UsableAddress(c.Pool.End) || c.Pool.Start.Compare(c.Pool.End) > 0 || netip.PrefixFrom(c.Pool.Start, int(bits)).Masked() != netip.PrefixFrom(c.Pool.End, int(bits)).Masked() {
			return errors.New("DHCPv6 pool requires an ordered IPv6 range within its prefix")
		}
	}
	if c.RefreshSeconds != 0 && c.RefreshSeconds < 600 {
		return errors.New("DHCPv6 information refresh must be at least 600 seconds")
	}
	if len(c.DNSServers) > 32 || len(c.SNTPServers) > 32 || len(c.DomainSearch) > 32 {
		return errors.New("DHCPv6 server supports at most 32 values per option")
	}
	for _, all := range [][]netip.Addr{c.DNSServers, c.SNTPServers} {
		for _, ip := range all {
			if !UsableAddress(ip) {
				return errors.New("DHCPv6 server option requires an unscoped IPv6 unicast address")
			}
		}
	}
	for _, name := range c.DomainSearch {
		if !validDomain(name) {
			return errors.New("invalid DHCPv6 search domain")
		}
	}
	return nil
}

func UsableAddress(ip netip.Addr) bool {
	return ip.Is6() && !ip.Is4In6() && ip.Zone() == "" && !ip.IsUnspecified() && !ip.IsMulticast()
}
func validDomain(name string) bool {
	if name == "" || len(name) > 253 || strings.ContainsAny(name, "\\\x00 \t\r\n") {
		return false
	}
	_, ok := dns.IsDomainName(dns.Fqdn(name))
	return ok
}

func ValidateInput(in Input) error {
	for _, grant := range []lifetime.Deadline{in.Authority, in.DNSReadyUntil} {
		if grant.Validate() != nil || grant.BootID == "" || !grant.UTC.IsZero() {
			return errors.New("DHCPv6 serving requires original boot authority and DNS readiness")
		}
	}
	if len(in.DNSServers) > 32 || len(in.SNTPServers) > 32 || len(in.DomainSearch) > 32 {
		return errors.New("too many learned DHCPv6 option values")
	}
	valid := func(d lifetime.Deadline) bool { return d.Validate() == nil && d.BootID != "" && d.UTC.IsZero() }
	for _, all := range [][]Address{in.DNSServers, in.SNTPServers} {
		for _, entry := range all {
			if !UsableAddress(entry.Address) || !valid(entry.ValidUntil) {
				return errors.New("invalid learned DHCPv6 address or deadline")
			}
		}
	}
	for _, entry := range in.DomainSearch {
		if !validDomain(entry.Name) || !valid(entry.ValidUntil) {
			return errors.New("invalid learned DHCPv6 domain or deadline")
		}
	}
	return nil
}

// ValidateSnapshot checks the current internal producer contract. Information-only
// services have no address leases or lease-observation lifetime.
func ValidateSnapshot(c Config, s Snapshot, clock boottime.Clock) error {
	if s.Resource != c.Resource || s.Interface != c.Interface {
		return errors.New("DHCPv6 server snapshot identity mismatch")
	}
	if _, err := dhcp6config.ParseDUID(s.ServerDUID); err != nil {
		return err
	}
	if c.Pool == nil {
		if s.Active || len(s.Leases) != 0 {
			return errors.New("information-only DHCPv6 server published address leases")
		}
		return nil
	}
	now := uint64(clock.BoottimeNanoseconds)
	if s.BootID != clock.BootID || s.ObservedNanos == 0 || s.ObservedNanos > now || now >= s.ExpiresNanos || s.ExpiresNanos-s.ObservedNanos > uint64(ObservationLifetime) || s.ObservedAt.IsZero() {
		return errors.New("DHCPv6 lease observation expired or has an invalid original clock")
	}
	if len(s.Leases) > MaxLeases || !s.Active && len(s.Leases) > 0 {
		return errors.New("DHCPv6 lease observation exceeds its serving scope")
	}
	seen := make(map[netip.Addr]bool, len(s.Leases))
	for _, l := range s.Leases {
		if !l.Address.Is6() || l.Address.Is4In6() || l.Address.Zone() != "" || !l.Address.IsGlobalUnicast() || seen[l.Address] || !l.ExpiresAt.After(s.ObservedAt) {
			return errors.New("DHCPv6 lease observation requires unique active IPv6 bindings")
		}
		seen[l.Address] = true
	}
	return nil
}
