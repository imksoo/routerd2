// SPDX-License-Identifier: BSD-3-Clause

// Package dhcpserverconfig describes committed DHCP server intent without
// importing sockets or worker lifecycle code into PID 1.
package dhcpserverconfig

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
)

type Option = api.DHCPv4OptionSpec
type Match = api.DHCPv4ServerScopeMatchSpec

const (
	DefaultLeaseSeconds = 12 * 60 * 60
	MaxSnapshotBytes    = 1792 * 1024
)

// Config is one server on one Ethernet interface in the worker's network
// namespace. The compiler resolves all resource/status dependencies first.
// The worker verifies that ServerAddress is already on Interface and that its
// VRF membership is exact; it never creates addresses or changes routing.
type Config struct {
	Resource       string        `json:"resource"`
	Interface      string        `json:"interface"`
	VRF            string        `json:"vrf,omitempty"`
	ServerAddress  string        `json:"server_address"`
	Primary        Scope         `json:"primary"`
	Scopes         []Scope       `json:"scopes,omitempty"`
	Reservations   []Reservation `json:"reservations,omitempty"`
	Authoritative  bool          `json:"authoritative,omitempty"`
	StickyHoldDays int           `json:"sticky_hold_days,omitempty"`
	LogDHCP        bool          `json:"log_dhcp,omitempty"`
	// Relay requests are accepted only from these exact IPv4 source addresses,
	// with giaddr also in this list and a matching configured pool subnet.
	TrustedRelays  []string `json:"trusted_relays,omitempty"`
	OfferSeconds   int      `json:"offer_seconds,omitempty"`
	DeclineSeconds int      `json:"decline_seconds,omitempty"`
	MaxLeases      int      `json:"max_leases,omitempty"`
	// Zero selects 300 ms. A negative value is invalid: conflict detection
	// cannot silently be disabled for newly allocated on-link addresses.
	ProbeTimeoutMilliseconds  int          `json:"probe_timeout_milliseconds,omitempty"`
	EffectTimeoutMilliseconds int          `json:"effect_timeout_milliseconds,omitempty"`
	StatePath                 string       `json:"state_path"`
	SocketPath                string       `json:"socket_path,omitempty"`
	StatusPath                string       `json:"status_path,omitempty"`
	EventPath                 string       `json:"event_path,omitempty"`
	Replication               *Replication `json:"replication,omitempty"`
	// Standby owns the journal and replication listener, without DHCP sockets.
	Standby bool `json:"standby,omitempty"`
}

type Replication struct {
	ListenAddress       netip.AddrPort `json:"listen_address"`
	PeerAddress         netip.AddrPort `json:"peer_address"`
	CertificateFile     string         `json:"certificate_file"`
	PrivateKeyFile      string         `json:"private_key_file"`
	PeerCertificateFile string         `json:"peer_certificate_file"`
}

type Scope struct {
	Name         string   `json:"name"`
	Prefix       string   `json:"prefix"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	LeaseSeconds int      `json:"lease_seconds,omitempty"`
	Router       string   `json:"router,omitempty"`
	DNSServers   []string `json:"dns_servers,omitempty"`
	NTPServers   []string `json:"ntp_servers,omitempty"`
	Domain       string   `json:"domain,omitempty"`
	Options      []Option `json:"options,omitempty"`
	Match        Match    `json:"match,omitempty"`
}

type Reservation struct {
	Scope        string   `json:"scope,omitempty"`
	MACAddress   string   `json:"mac_address"`
	IPAddress    string   `json:"ip_address"`
	Hostname     string   `json:"hostname,omitempty"`
	LeaseSeconds int      `json:"lease_seconds,omitempty"`
	Options      []Option `json:"options,omitempty"`
}

func Normalize(c Config) Config {
	out := c
	out.Scopes = slices.Clone(c.Scopes)
	out.Reservations = slices.Clone(c.Reservations)
	out.TrustedRelays = slices.Clone(c.TrustedRelays)
	out.Primary = cloneScope(c.Primary)
	for i := range out.Scopes {
		out.Scopes[i] = cloneScope(out.Scopes[i])
	}
	for i := range out.Reservations {
		out.Reservations[i].Options = slices.Clone(out.Reservations[i].Options)
	}
	if c.Replication != nil {
		r := *c.Replication
		out.Replication = &r
	}
	if out.Primary.Name == "" {
		out.Primary.Name = "default"
	}
	if out.Primary.LeaseSeconds == 0 {
		out.Primary.LeaseSeconds = DefaultLeaseSeconds
	}
	for i := range out.Scopes {
		if out.Scopes[i].LeaseSeconds == 0 {
			out.Scopes[i].LeaseSeconds = out.Primary.LeaseSeconds
		}
	}
	if out.OfferSeconds == 0 {
		out.OfferSeconds = 60
	}
	if out.DeclineSeconds == 0 {
		out.DeclineSeconds = 600
	}
	if out.MaxLeases == 0 {
		out.MaxLeases = 1024
	}
	if out.ProbeTimeoutMilliseconds == 0 {
		out.ProbeTimeoutMilliseconds = 300
	}
	if out.EffectTimeoutMilliseconds == 0 {
		out.EffectTimeoutMilliseconds = 2000
	}
	for i := range out.Reservations {
		if mac, err := net.ParseMAC(out.Reservations[i].MACAddress); err == nil {
			out.Reservations[i].MACAddress = mac.String()
		}
	}
	return out
}

func cloneScope(s Scope) Scope {
	s.DNSServers = slices.Clone(s.DNSServers)
	s.NTPServers = slices.Clone(s.NTPServers)
	s.Options = slices.Clone(s.Options)
	s.Match.MACAddresses = slices.Clone(s.Match.MACAddresses)
	s.Match.OUIPrefixes = slices.Clone(s.Match.OUIPrefixes)
	return s
}

func Validate(c Config) error {
	c = Normalize(c)
	if c.Standby && c.Replication == nil {
		return errors.New("DHCP standby requires replication")
	}
	if r := c.Replication; r != nil {
		for _, endpoint := range []netip.AddrPort{r.ListenAddress, r.PeerAddress} {
			if !endpoint.IsValid() || endpoint.Port() == 0 || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() {
				return errors.New("DHCP replication requires explicit local and peer unicast endpoints")
			}
		}
		if r.ListenAddress == r.PeerAddress {
			return errors.New("DHCP replication endpoints must differ")
		}
		for _, p := range []string{r.CertificateFile, r.PrivateKeyFile, r.PeerCertificateFile} {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsRune(p, 0) {
				return errors.New("DHCP replication credentials require absolute file paths")
			}
		}
	}
	if !validName(c.Resource, 256) || !ValidInterface(c.Interface) || (c.VRF != "" && !ValidInterface(c.VRF)) || c.VRF == c.Interface {
		return errors.New("DHCP server requires a bounded resource identity, interface and optional distinct VRF")
	}
	server, err := ipv4(c.ServerAddress)
	if err != nil {
		return errors.New("DHCP server_address must be unicast IPv4")
	}
	if c.MaxLeases < 1 || c.MaxLeases > 2048 {
		return errors.New("DHCP max_leases must be 1..2048; snapshots are also capped at 1792 KiB")
	}
	if c.StickyHoldDays < 0 || c.StickyHoldDays > 3650 {
		return errors.New("DHCP sticky_hold_days must be 0..3650")
	}
	if c.OfferSeconds < 1 || c.OfferSeconds > 600 || c.DeclineSeconds < 60 || c.DeclineSeconds > 604800 {
		return errors.New("DHCP offer_seconds must be 1..600 and decline_seconds 60..604800")
	}
	if c.ProbeTimeoutMilliseconds < 50 || c.ProbeTimeoutMilliseconds > 2000 || c.EffectTimeoutMilliseconds < 10 || c.EffectTimeoutMilliseconds > 4000 {
		return errors.New("DHCP probe timeout must be 50..2000 ms and effect timeout 10..4000 ms")
	}
	if len(c.Scopes) > 64 || len(c.Reservations) > c.MaxLeases || len(c.TrustedRelays) > 128 {
		return errors.New("DHCP scope, reservation or relay limit exceeded")
	}
	paths := map[string]string{}
	for name, p := range map[string]string{"state_path": c.StatePath, "socket_path": c.SocketPath, "status_path": c.StatusPath, "event_path": c.EventPath} {
		if p == "" && name != "state_path" {
			continue
		}
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsRune(p, 0) {
			return fmt.Errorf("DHCP %s must be a clean absolute file path", name)
		}
		if name == "socket_path" && len(p) > 100 {
			return errors.New("DHCP socket_path is too long")
		}
		for _, owned := range []string{p, p + ".lock", p + ".1"} {
			if prev, ok := paths[owned]; ok {
				return fmt.Errorf("DHCP %s overlaps %s", name, prev)
			}
			paths[owned] = name
		}
	}
	scopes := append([]Scope{c.Primary}, c.Scopes...)
	names := map[string]Scope{}
	selectors := map[string]string{}
	for i, s := range scopes {
		if !validName(s.Name, 64) || names[s.Name].Name != "" {
			return errors.New("DHCP scope names must be unique bounded identities")
		}
		names[s.Name] = s
		p, e := netip.ParsePrefix(s.Prefix)
		if e != nil || !p.Addr().Is4() || p.Bits() > 30 || p.Bits() < 1 || p != p.Masked() {
			return fmt.Errorf("DHCP scope %s prefix must be a canonical IPv4 /1../30 subnet", s.Name)
		}
		start, e1 := ipv4(s.Start)
		end, e2 := ipv4(s.End)
		if e1 != nil || e2 != nil || !Usable(p, start) || !Usable(p, end) || start.Compare(end) > 0 {
			return fmt.Errorf("DHCP scope %s pool must be an ordered range of usable subnet addresses", s.Name)
		}
		if s.LeaseSeconds < 60 || uint64(s.LeaseSeconds) > uint64(^uint32(0)-1) {
			return errors.New("DHCP lease_seconds must be 60..4294967294 (infinite leases are not supported)")
		}
		if server == start && start == end {
			return errors.New("DHCP pool cannot contain only the server address")
		}
		if s.Router != "" {
			r, e := ipv4(s.Router)
			if e != nil || !Usable(p, r) {
				return fmt.Errorf("DHCP scope %s router must be on its subnet", s.Name)
			}
		}
		for _, list := range [][]string{s.DNSServers, s.NTPServers} {
			if len(list) > 16 {
				return errors.New("DHCP DNS/NTP server lists are limited to 16 addresses")
			}
			for _, ip := range list {
				if _, e := ipv4(ip); e != nil {
					return errors.New("DHCP DNS/NTP servers must be unicast IPv4")
				}
			}
		}
		if s.Domain != "" && !ValidHostname(s.Domain) {
			return errors.New("DHCP domain is invalid")
		}
		if _, e := EncodeOptions(s.Options); e != nil {
			return fmt.Errorf("DHCP scope %s: %w", s.Name, e)
		}
		if i == 0 && (len(s.Match.MACAddresses) != 0 || len(s.Match.OUIPrefixes) != 0) {
			return errors.New("DHCP primary pool cannot have a selector")
		}
		if i > 0 && len(s.Match.MACAddresses) == 0 && len(s.Match.OUIPrefixes) == 0 {
			return errors.New("DHCP secondary scopes require MAC or OUI selectors")
		}
		if len(s.Match.MACAddresses)+len(s.Match.OUIPrefixes) > 2048 {
			return errors.New("DHCP scope selector limit exceeded")
		}
		for _, v := range s.Match.MACAddresses {
			m, e := EthernetMAC(v)
			if e != nil {
				return e
			}
			key := "mac:" + m.String()
			if prev := selectors[key]; prev != "" {
				return fmt.Errorf("DHCP selector is shared by scopes %s and %s", prev, s.Name)
			}
			selectors[key] = s.Name
		}
		for _, v := range s.Match.OUIPrefixes {
			m, e := ParseOUI(v)
			if e != nil {
				return e
			}
			key := "oui:" + m.String()
			if prev := selectors[key]; prev != "" {
				return fmt.Errorf("DHCP selector is shared by scopes %s and %s", prev, s.Name)
			}
			selectors[key] = s.Name
		}
	}
	macs, ips := map[string]bool{}, map[string]bool{}
	for _, r := range c.Reservations {
		m, e := EthernetMAC(r.MACAddress)
		if e != nil {
			return e
		}
		a, e := ipv4(r.IPAddress)
		if e != nil {
			return errors.New("DHCP reservation address must be unicast IPv4")
		}
		if macs[m.String()] || ips[a.String()] {
			return errors.New("DHCP reservations require unique MAC and IP addresses")
		}
		macs[m.String()], ips[a.String()] = true, true
		s := SelectScope(c, m)
		if r.Scope != "" {
			var ok bool
			s, ok = names[r.Scope]
			if !ok {
				return errors.New("DHCP reservation references an unknown scope")
			}
		}
		p, _ := netip.ParsePrefix(s.Prefix)
		if !Usable(p, a) || a == server {
			return errors.New("DHCP reservation must be a usable address in its scope and cannot be the server address")
		}
		if InfrastructureAddress(c, a.String()) {
			return errors.New("DHCP reservation cannot assign the server or a configured router address")
		}
		if r.Hostname != "" && !ValidHostname(r.Hostname) {
			return errors.New("DHCP reservation hostname is invalid")
		}
		if r.LeaseSeconds != 0 && (r.LeaseSeconds < 60 || uint64(r.LeaseSeconds) > uint64(^uint32(0)-1)) {
			return errors.New("DHCP reservation lease_seconds must be 60..4294967294")
		}
		if _, e := EncodeOptions(r.Options); e != nil {
			return fmt.Errorf("DHCP reservation: %w", e)
		}
	}
	relays := map[string]bool{}
	for _, v := range c.TrustedRelays {
		a, e := ipv4(v)
		if e != nil || relays[a.String()] {
			return errors.New("DHCP trusted_relays must contain distinct unicast IPv4 addresses")
		}
		relays[a.String()] = true
	}
	return nil
}

func ValidInterface(v string) bool {
	return v != "" && len(v) <= 15 && v != "." && v != ".." && !strings.ContainsAny(v, "/\x00 \t\r\n:")
}
func validName(v string, max int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= max && !strings.ContainsAny(v, "\x00\r\n")
}
func ipv4(v string) (netip.Addr, error) {
	a, e := netip.ParseAddr(v)
	if e != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return netip.Addr{}, errors.New("invalid unicast IPv4")
	}
	return a, nil
}
func IPv4(v string) (netip.Addr, error) { return ipv4(v) }
func Usable(p netip.Prefix, a netip.Addr) bool {
	if !a.Is4() || !p.Contains(a) || p.Masked().Addr() == a {
		return false
	}
	n := a.As4()
	v := uint32(n[0])<<24 | uint32(n[1])<<16 | uint32(n[2])<<8 | uint32(n[3])
	host := uint32(0xffffffff) >> p.Bits()
	return v&host != host
}
func EthernetMAC(v string) (net.HardwareAddr, error) {
	m, e := net.ParseMAC(v)
	if e != nil || len(m) != 6 || m[0]&1 != 0 || m.String() == "00:00:00:00:00:00" {
		return nil, errors.New("DHCP requires a nonzero unicast Ethernet MAC")
	}
	return m, nil
}
func ParseOUI(v string) (net.HardwareAddr, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), "-", ":")
	if len(v) != 8 {
		return nil, errors.New("DHCP OUI must contain three hexadecimal octets")
	}
	m, e := net.ParseMAC(v + ":00:00:00")
	if e != nil || m[0]&1 != 0 {
		return nil, errors.New("DHCP OUI must be unicast")
	}
	return m[:3], nil
}
func ValidHostname(v string) bool {
	v = strings.TrimSuffix(v, ".")
	if v == "" || len(v) > 253 {
		return false
	}
	for _, label := range strings.Split(v, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// Exact MAC selectors take precedence over OUI selectors. Duplicate selectors
// between scopes are rejected during validation, so ordering cannot change it.
func SelectScope(c Config, mac net.HardwareAddr) Scope {
	for _, s := range c.Scopes {
		for _, v := range s.Match.MACAddresses {
			m, _ := net.ParseMAC(v)
			if string(m) == string(mac) {
				return s
			}
		}
	}
	for _, s := range c.Scopes {
		for _, v := range s.Match.OUIPrefixes {
			m, _ := ParseOUI(v)
			if len(mac) >= 3 && string(m) == string(mac[:3]) {
				return s
			}
		}
	}
	return c.Primary
}

// InfrastructureAddress includes routers expressed with the original option
// API, including multi-router option 3 overrides. Such addresses never enter
// a dynamic pool merely because they were not in the shorthand Router field.
func InfrastructureAddress(c Config, address string) bool {
	if address == c.ServerAddress {
		return true
	}
	check := func(options []Option) bool {
		for _, o := range options {
			if o.Code != 3 && !strings.EqualFold(strings.TrimSpace(o.Name), "router") {
				continue
			}
			encoded, err := EncodeOptions([]Option{o})
			if err != nil {
				continue
			}
			b := encoded[3]
			for len(b) >= 4 {
				var raw [4]byte
				copy(raw[:], b[:4])
				if netip.AddrFrom4(raw).String() == address {
					return true
				}
				b = b[4:]
			}
		}
		return false
	}
	if c.Primary.Router == address || check(c.Primary.Options) {
		return true
	}
	for _, s := range c.Scopes {
		if s.Router == address || check(s.Options) {
			return true
		}
	}
	for _, r := range c.Reservations {
		if check(r.Options) {
			return true
		}
	}
	return false
}
