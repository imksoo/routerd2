// Package dhcp6serverconfig describes LAN DHCPv6 information service intent.
package dhcp6serverconfig

import (
	"errors"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/miekg/dns"
)

type Config struct {
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
	Authority    lifetime.Deadline `json:"authority"`
	DNSServers   []Address         `json:"dns_servers,omitempty"`
	SNTPServers  []Address         `json:"sntp_servers,omitempty"`
	DomainSearch []Domain          `json:"domain_search,omitempty"`
}
type Snapshot struct {
	Resource   string `json:"resource"`
	Interface  string `json:"interface"`
	ServerDUID string `json:"server_duid"`
}

func Validate(c Config) error {
	if c.Resource == "" || c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") || !filepath.IsAbs(c.StatePath) {
		return errors.New("DHCPv6 server requires a resource, interface and private state path")
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
	if in.Authority.Validate() != nil || in.Authority.BootID == "" || !in.Authority.UTC.IsZero() {
		return errors.New("DHCPv6 serving requires original boot authority")
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
