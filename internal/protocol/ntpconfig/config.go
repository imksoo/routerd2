// SPDX-License-Identifier: BSD-3-Clause

// Package ntpconfig defines time-service intent and observations without linking
// the chrony process or its command protocol into the supervisor.
package ntpconfig

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
	mdns "github.com/miekg/dns"
)

type Server struct {
	Address string `json:"address"`
	Port    uint16 `json:"port,omitempty"`
}

type Node struct {
	Servers     []Server       `json:"servers"`
	SourcesFrom []string       `json:"sources_from,omitempty"`
	DNS         []netip.Addr   `json:"dns,omitempty"`
	Listen      []netip.Addr   `json:"listen,omitempty"`
	Allow       []netip.Prefix `json:"allow,omitempty"`
}

// LearnedServer retains the supplying protocol's original information lifetime.
type LearnedServer struct {
	Server
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type Sources struct {
	ConfigDigest string          `json:"config_digest"`
	Generation   uint64          `json:"generation"`
	Servers      []LearnedServer `json:"servers"`
}

type Config struct {
	Node
	StateDir   string `json:"state_dir"`
	RuntimeDir string `json:"runtime_dir"`
}

type Snapshot struct {
	BootID         string     `json:"boot_id"`
	ObservedNanos  uint64     `json:"observed_nanos"`
	ExpiresNanos   uint64     `json:"expires_nanos"`
	Synchronized   bool       `json:"synchronized"`
	AdjustingClock bool       `json:"adjusting_clock"`
	Source         netip.Addr `json:"source,omitempty"`
	Stratum        uint16     `json:"stratum"`
	LeapStatus     uint16     `json:"leap_status"`
	ReferenceTime  time.Time  `json:"reference_time"`
	OffsetSeconds  float64    `json:"offset_seconds"`
	RMSOffset      float64    `json:"rms_offset_seconds"`
	FrequencyPPM   float64    `json:"frequency_ppm"`
	SkewPPM        float64    `json:"skew_ppm"`
	RootDelay      float64    `json:"root_delay_seconds"`
	RootDispersion float64    `json:"root_dispersion_seconds"`
	LastError      string     `json:"last_error,omitempty"`
}

func ValidateNode(n Node) error {
	if len(n.Servers) > 32 || len(n.SourcesFrom) > 32 || len(n.DNS) > 8 || len(n.Allow) > 128 {
		return errors.New("NTP exceeds source, resolver or access-list bounds")
	}
	for _, server := range n.Servers {
		if err := ValidateServer(server, len(n.DNS) > 0); err != nil {
			return err
		}
	}
	families := map[bool]bool{}
	for _, address := range n.Listen {
		if !usableAddress(address) || families[address.Is4()] {
			return errors.New("chronyd accepts one unicast listen address per family")
		}
		families[address.Is4()] = true
	}
	if len(n.Allow) > 0 && len(n.Listen) == 0 {
		return errors.New("NTP server access requires explicit listen addresses")
	}
	for _, prefix := range n.Allow {
		if !prefix.IsValid() {
			return errors.New("NTP allow entries must be valid IP prefixes")
		}
	}
	for _, address := range n.DNS {
		if !usableAddress(address) {
			return errors.New("NTP resolver must be a usable IP address")
		}
	}
	return nil
}

func ValidateServer(server Server, hasDNS bool) error {
	if address, err := netip.ParseAddr(server.Address); err == nil {
		if !usableAddress(address) {
			return errors.New("NTP source must be unicast and link-local sources need an interface zone")
		}
		return nil
	}
	if _, ok := mdns.IsDomainName(server.Address); !ok || server.Address == "" || strings.ContainsAny(server.Address, "\x00 \t\r\n\\#;") {
		return errors.New("NTP source must be an IP address or hostname")
	}
	if !hasDNS {
		return errors.New("NTP hostname sources require explicit DNS resolvers for the private engine")
	}
	return nil
}

func Validate(c Config) error {
	if err := ValidateNode(c.Node); err != nil {
		return err
	}
	for _, path := range []string{c.StateDir, c.RuntimeDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("NTP state and runtime directories must be clean absolute private paths")
		}
	}
	return nil
}

// Render retains routerd's server/iburst, allow and bindaddress behavior. The
// single foreground engine also owns persistent drift and a private Unix-only
// command socket; no service manager or remotely exposed command port is used.
func Render(n Node) []byte {
	var b bytes.Buffer
	b.WriteString("driftfile /state/drift\ndumpdir /state\npidfile /run/chronyd.pid\nbindcmdaddress /run/chronyd.sock\ncmdport 0\nmakestep 1.0 3\nsourcedir /run/sources\n")
	if len(n.SourcesFrom) == 0 {
		b.Write(RenderServers(n.Servers))
	}
	if len(n.Listen) == 0 {
		b.WriteString("port 0\n")
	}
	for _, address := range n.Listen {
		fmt.Fprintf(&b, "bindaddress %s\n", address)
	}
	for _, prefix := range n.Allow {
		fmt.Fprintf(&b, "allow %s\n", prefix.Masked())
	}
	return b.Bytes()
}

func RenderServers(servers []Server) []byte {
	var b bytes.Buffer
	for _, server := range servers {
		port := server.Port
		if port == 0 {
			port = 123
		}
		fmt.Fprintf(&b, "server %s port %d iburst\n", server.Address, port)
	}
	return b.Bytes()
}

func usableAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() && address.Zone() == "" {
		return false
	}
	zone := address.Zone()
	return len(zone) <= 15 && !strings.ContainsAny(zone, "/\x00 \t\r\n#;")
}
