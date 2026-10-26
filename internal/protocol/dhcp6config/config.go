// Package dhcp6config defines DHCPv6 client intent and learned assignments.
package dhcp6config

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Resource       string `json:"resource"`
	Interface      string `json:"interface"`
	StatePath      string `json:"state_path"`
	ClientDUID     string `json:"client_duid,omitempty"`
	IAID           uint32 `json:"iaid"`
	RequestAddress bool   `json:"request_address,omitempty"`
	RequestPrefix  bool   `json:"request_prefix,omitempty"`
	PrefixLength   int    `json:"prefix_length,omitempty"`
}

type Address struct {
	Address        string    `json:"address"`
	PreferredUntil time.Time `json:"preferred_until"`
	ValidUntil     time.Time `json:"valid_until"`
}
type Prefix struct {
	Prefix         string    `json:"prefix"`
	PreferredUntil time.Time `json:"preferred_until"`
	ValidUntil     time.Time `json:"valid_until"`
}

type Snapshot struct {
	Resource                  string    `json:"resource"`
	Interface                 string    `json:"interface"`
	IAID                      uint32    `json:"iaid"`
	ClientDUID                string    `json:"client_duid"`
	ServerDUID                string    `json:"server_duid,omitempty"`
	State                     string    `json:"state"`
	Addresses                 []Address `json:"addresses,omitempty"`
	Prefixes                  []Prefix  `json:"prefixes,omitempty"`
	RenewAt                   time.Time `json:"renew_at,omitempty"`
	RebindAt                  time.Time `json:"rebind_at,omitempty"`
	DNSServers                []string  `json:"dns_servers,omitempty"`
	DomainSearch              []string  `json:"domain_search,omitempty"`
	SNTPServers               []string  `json:"sntp_servers,omitempty"`
	AFTRName                  string    `json:"aftr_name,omitempty"`
	InfoUpdatedAt             time.Time `json:"info_updated_at,omitempty"`
	InformationRefreshSeconds uint32    `json:"information_refresh_seconds,omitempty"`
	InformationRefreshAt      time.Time `json:"information_refresh_at,omitempty"`
}

func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" || c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") || !filepath.IsAbs(c.StatePath) {
		return errors.New("DHCPv6 requires a resource, valid interface and absolute state_path")
	}
	if c.PrefixLength < 0 || c.PrefixLength > 128 || !c.RequestPrefix && c.PrefixLength != 0 {
		return errors.New("DHCPv6 prefix_length requires request_prefix and must be 1..128 or zero for /60")
	}
	if c.ClientDUID != "" {
		_, err := ParseDUID(c.ClientDUID)
		return err
	}
	return nil
}
func ParseDUID(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) < 4 || len(b) > 128 {
		return nil, errors.New("client_duid must be 4..128 bytes of hexadecimal DUID")
	}
	return b, nil
}

// LeaseUntil bounds consumers by the last surviving assignment. Information
// refresh is separate and cannot renew an address or a delegated prefix.
func (s Snapshot) LeaseUntil() time.Time {
	var until time.Time
	for _, a := range s.Addresses {
		if a.ValidUntil.After(until) {
			until = a.ValidUntil
		}
	}
	for _, p := range s.Prefixes {
		if p.ValidUntil.After(until) {
			until = p.ValidUntil
		}
	}
	return until
}
func (s Snapshot) Active(now time.Time) bool {
	if s.State == "stopped" {
		return false
	}
	if s.State == "information" {
		_, ok := s.InformationUntil(now)
		return ok
	}
	return now.Before(s.LeaseUntil())
}
func (s Snapshot) InformationUntil(now time.Time) (time.Time, bool) {
	if s.State == "stopped" || s.InfoUpdatedAt.IsZero() {
		return time.Time{}, false
	}
	until := s.InformationRefreshAt
	if until.IsZero() && s.InformationRefreshSeconds != ^uint32(0) {
		return time.Time{}, false
	}
	if s.State != "information" {
		lease := s.LeaseUntil()
		if lease.IsZero() {
			return time.Time{}, false
		}
		if until.IsZero() || lease.Before(until) {
			until = lease
		}
	}
	return until, until.IsZero() || now.Before(until)
}
func ValidateSnapshot(c Config, s Snapshot) error {
	if s.Resource != c.Resource || s.Interface != c.Interface || s.IAID != c.IAID {
		return errors.New("DHCPv6 snapshot identity mismatch")
	}
	if _, err := ParseDUID(s.ClientDUID); err != nil {
		return err
	}
	switch s.State {
	case "soliciting", "requesting", "bound", "renewing", "rebinding", "declining", "information", "stopped":
	default:
		return errors.New("invalid DHCPv6 client state")
	}
	if s.State == "stopped" && (len(s.Addresses)+len(s.Prefixes) > 0 || !s.InfoUpdatedAt.IsZero()) {
		return errors.New("stopped DHCPv6 client retains assignments/information")
	}
	if s.State == "information" && (c.RequestAddress || c.RequestPrefix) {
		return errors.New("DHCPv6 state does not match requested associations")
	}
	if s.ServerDUID != "" {
		if _, err := ParseDUID(s.ServerDUID); err != nil {
			return err
		}
	}
	if len(s.Addresses) > 64 || len(s.Prefixes) > 64 {
		return errors.New("too many DHCPv6 assignments")
	}
	seen := map[string]bool{}
	for _, a := range s.Addresses {
		ip, err := netip.ParseAddr(a.Address)
		if err != nil || !usable(ip) || a.Address != ip.String() || !c.RequestAddress || seen[a.Address] || a.ValidUntil.IsZero() || a.PreferredUntil.IsZero() || a.PreferredUntil.After(a.ValidUntil) {
			return errors.New("invalid DHCPv6 address assignment")
		}
		seen[a.Address] = true
	}
	for _, p := range s.Prefixes {
		ip, err := netip.ParsePrefix(p.Prefix)
		if err != nil || !usable(ip.Addr()) || ip != ip.Masked() || p.Prefix != ip.String() || !c.RequestPrefix || seen[p.Prefix] || p.ValidUntil.IsZero() || p.PreferredUntil.IsZero() || p.PreferredUntil.After(p.ValidUntil) {
			return errors.New("invalid DHCPv6 prefix assignment")
		}
		seen[p.Prefix] = true
	}
	return nil
}
func usable(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLinkLocalUnicast()
}
