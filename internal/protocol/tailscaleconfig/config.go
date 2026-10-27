// Package tailscaleconfig contains operator intent and observations, without
// importing the Tailscale engine into boot or configuration validation.
package tailscaleconfig

import (
	"errors"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/lifetime"
)

// Node owns one Tailscale TUN and its engine's dedicated Linux tables. Ordinary
// routerd firewall policy remains independently owned by the network writer.
type Node struct {
	Interface         string         `json:"interface"`
	ServerURL         string         `json:"server_url"`
	Hostname          string         `json:"hostname,omitempty"`
	AuthKeyFile       string         `json:"auth_key_file,omitempty"`
	ControlCAFile     string         `json:"control_ca_file,omitempty"`
	BootstrapDNS      []netip.Addr   `json:"bootstrap_dns,omitempty"`
	AdvertiseRoutes   []netip.Prefix `json:"advertise_routes,omitempty"`
	AdvertiseExitNode bool           `json:"advertise_exit_node,omitempty"`
	AdvertiseTags     []string       `json:"advertise_tags,omitempty"`
	AcceptRoutes      bool           `json:"accept_routes,omitempty"`
	ShieldsUp         bool           `json:"shields_up,omitempty"`
}

type Input struct {
	ConfigDigest string            `json:"config_digest"`
	Authority    lifetime.Deadline `json:"authority"`
}

type Config struct {
	Node
	Resource   string `json:"resource"`
	StateDir   string `json:"state_dir"`
	RuntimeDir string `json:"runtime_dir"`
}

type Peer struct {
	ID              string         `json:"id"`
	Hostname        string         `json:"hostname,omitempty"`
	Online          bool           `json:"online"`
	Addresses       []netip.Addr   `json:"addresses,omitempty"`
	AllowedPrefixes []netip.Prefix `json:"allowed_prefixes,omitempty"`
	HomeDERP        string         `json:"home_derp,omitempty"`
	DirectAddress   string         `json:"direct_address,omitempty"`
	RxBytes         int64          `json:"rx_bytes"`
	TxBytes         int64          `json:"tx_bytes"`
}

type Snapshot struct {
	Resource      string       `json:"resource"`
	Interface     string       `json:"interface"`
	BackendState  string       `json:"backend_state"`
	NodeID        string       `json:"node_id,omitempty"`
	Online        bool         `json:"online"`
	Addresses     []netip.Addr `json:"addresses,omitempty"`
	Peers         []Peer       `json:"peers,omitempty"`
	BootID        string       `json:"boot_id"`
	ObservedNanos uint64       `json:"observed_nanos"`
	ExpiresNanos  uint64       `json:"expires_nanos"`
}

func ValidateNode(n Node) error {
	if n.Interface == "" || len(n.Interface) > 15 || strings.ContainsAny(n.Interface, "/\x00 \t\r\n") {
		return errors.New("Tailscale requires an owned TUN interface name")
	}
	u, err := url.Parse(n.ServerURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("Tailscale requires an HTTP(S) control server URL")
	}
	if n.AuthKeyFile != "" && (!filepath.IsAbs(n.AuthKeyFile) || filepath.Clean(n.AuthKeyFile) != n.AuthKeyFile) {
		return errors.New("Tailscale auth_key_file must be an absolute private file")
	}
	if u.Scheme == "https" && n.ControlCAFile == "" {
		return errors.New("Tailscale HTTPS control server requires an explicit control_ca_file trust bundle")
	}
	if n.ControlCAFile != "" && !filepath.IsAbs(n.ControlCAFile) {
		return errors.New("Tailscale control_ca_file must be absolute")
	}
	if _, err := netip.ParseAddr(u.Hostname()); err != nil && len(n.BootstrapDNS) == 0 {
		return errors.New("Tailscale hostname control server requires explicit bootstrap_dns")
	}
	for _, a := range n.BootstrapDNS {
		if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() {
			return errors.New("invalid Tailscale bootstrap DNS server")
		}
	}
	for _, p := range n.AdvertiseRoutes {
		if !p.IsValid() || p.Addr().Is4In6() {
			return errors.New("invalid Tailscale advertised prefix")
		}
	}
	for _, tag := range n.AdvertiseTags {
		if !strings.HasPrefix(tag, "tag:") || len(tag) == 4 || strings.ContainsAny(tag, " ,\x00\t\r\n") {
			return errors.New("Tailscale advertised tags require tag: names")
		}
	}
	return nil
}

func Validate(c Config) error {
	if err := ValidateNode(c.Node); err != nil {
		return err
	}
	if c.Resource == "" || !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.RuntimeDir) {
		return errors.New("Tailscale requires private resource state and runtime directories")
	}
	return nil
}
