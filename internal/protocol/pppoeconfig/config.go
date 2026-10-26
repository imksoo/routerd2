// Package pppoeconfig defines PPP intent and finite session observations without
// importing the operating-system PPP engine into the control or boot path.
package pppoeconfig

import (
	"errors"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/lifetime"
)

type Config struct {
	Resource        string `json:"resource,omitempty"`
	Interface       string `json:"interface"`
	PPPInterface    string `json:"ppp_interface"`
	Username        string `json:"username"`
	PasswordFile    string `json:"password_file"`
	AuthMethod      string `json:"auth_method,omitempty"`
	ServiceName     string `json:"service_name,omitempty"`
	ACName          string `json:"ac_name,omitempty"`
	MTU             int    `json:"mtu,omitempty"`
	MRU             int    `json:"mru,omitempty"`
	LCPEchoInterval int    `json:"lcp_echo_interval,omitempty"`
	LCPEchoFailure  int    `json:"lcp_echo_failure,omitempty"`
	IPv6            bool   `json:"ipv6,omitempty"`
	DefaultRoute    *bool  `json:"default_route,omitempty"`
	Metric          int    `json:"metric,omitempty"`
	Table           int    `json:"table,omitempty"`
}

type Snapshot struct {
	Resource     string            `json:"resource"`
	Interface    string            `json:"interface"`
	PPPInterface string            `json:"ppp_interface"`
	Active       bool              `json:"active"`
	Address      string            `json:"address,omitempty"`
	Peer         string            `json:"peer,omitempty"`
	DNSServers   []string          `json:"dns_servers,omitempty"`
	ValidUntil   lifetime.Deadline `json:"valid_until,omitzero"`
}

func Validate(c Config) error {
	for _, name := range []string{c.Interface, c.PPPInterface} {
		if name == "" || len(name) > 15 {
			return errors.New("PPPoE requires valid physical and PPP interface names")
		}
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.') {
				return errors.New("PPPoE interface names must use letters, digits, underscores, hyphens or periods")
			}
		}
	}
	if c.Interface == c.PPPInterface || c.Username == "" || !filepath.IsAbs(c.PasswordFile) {
		return errors.New("PPPoE requires a distinct PPP interface, username and absolute password_file")
	}
	for _, v := range []string{c.Username, c.ServiceName, c.ACName} {
		if strings.ContainsAny(v, "\x00\r\n") {
			return errors.New("PPPoE option contains a line break or NUL")
		}
	}
	switch c.AuthMethod {
	case "", "pap", "chap", "both":
	default:
		return errors.New("PPPoE auth_method must be pap, chap or both")
	}
	for _, v := range []int{c.MTU, c.MRU} {
		if v != 0 && (v < 128 || v > 1492 || c.IPv6 && v < 1280) {
			return errors.New("PPPoE MTU/MRU must be 128..1492 (at least 1280 with IPv6)")
		}
	}
	if c.LCPEchoInterval < 0 || c.LCPEchoFailure < 0 || c.Metric < 0 || uint64(c.Metric) > 1<<32-1 || c.Table < 0 || uint64(c.Table) > 1<<32-1 || c.Table == 255 {
		return errors.New("invalid PPPoE echo or route settings")
	}
	return nil
}

func ValidateSnapshot(c Config, s Snapshot) error {
	if s.Resource != c.Resource || s.Interface != c.Interface || s.PPPInterface != c.PPPInterface {
		return errors.New("PPPoE observation identity mismatch")
	}
	if !s.Active {
		if s.Address != "" || s.Peer != "" || s.ValidUntil != (lifetime.Deadline{}) || len(s.DNSServers) != 0 {
			return errors.New("inactive PPPoE observation retains negotiated state")
		}
		return nil
	}
	for _, text := range append([]string{s.Address, s.Peer}, s.DNSServers...) {
		a, err := netip.ParseAddr(text)
		if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
			return errors.New("invalid negotiated PPP IPv4 address")
		}
	}
	if err := s.ValidUntil.Validate(); err != nil {
		return err
	}
	if s.ValidUntil.BootID == "" || s.ValidUntil.Nanos == 0 {
		return errors.New("active PPPoE observation requires a finite boot deadline")
	}
	return nil
}
