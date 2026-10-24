// SPDX-License-Identifier: BSD-3-Clause

// Package bgpconfig describes BGP intent without linking GoBGP, gRPC or its
// protocol workers into PID 1. bgpdaemon contains only the upstream applied
// configuration and local HTTP client types.
package bgpconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
)

type AppliedConfig = bgpdaemon.AppliedConfig
type AppliedGlobal = bgpdaemon.AppliedGlobal
type AppliedPeer = bgpdaemon.AppliedPeer
type AppliedPath = bgpdaemon.AppliedPath

// Config is resolved, committed intent. Runtime supplies the initial global,
// peers, policies and local paths. A persisted journal is reused only when its
// resource and normalized committed Runtime digest still match this Config.
// A module is one BGP instance in the worker's existing network namespace.
type Config struct {
	Resource                  string                   `json:"resource"`
	Runtime                   bgpdaemon.AppliedConfig  `json:"runtime"`
	SocketPath                string                   `json:"socket_path"`
	GRPCSocketPath            string                   `json:"grpc_socket_path,omitempty"`
	StatePath                 string                   `json:"state_path"`
	StatusPath                string                   `json:"status_path,omitempty"`
	EventPath                 string                   `json:"event_path,omitempty"`
	PeerTransports            map[string]PeerTransport `json:"peer_transports,omitempty"`
	PeerBFD                   map[string]BFDReference  `json:"peer_bfd,omitempty"`
	BindInterface             string                   `json:"bind_interface,omitempty"`
	FIBInterface              string                   `json:"fib_interface,omitempty"`
	Table                     int                      `json:"table,omitempty"`
	RouteMetric               int                      `json:"route_metric,omitempty"`
	MaxRoutes                 int                      `json:"max_routes,omitempty"`
	EffectTimeoutMilliseconds int                      `json:"effect_timeout_milliseconds,omitempty"`
}

// PeerTransport is additional native transport intent. LocalAddress and
// RemotePort are useful for cloud routing and isolated tests; BindInterface
// scopes sockets and link-local next hops. It can name a Linux VRF master.
type PeerTransport struct {
	LocalAddress  string `json:"local_address,omitempty"`
	RemotePort    int    `json:"remote_port,omitempty"`
	BindInterface string `json:"bind_interface,omitempty"`
	FIBInterface  string `json:"fib_interface,omitempty"`
}

func Normalize(c Config) Config {
	// Upstream Normalize canonicalizes slices in place. Clone first so a plan
	// or another module cannot observe mutation of its shared configuration.
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	out.Runtime = bgpdaemon.Normalize(out.Runtime)
	if out.Runtime.Global.ListenPort == 0 {
		out.Runtime.Global.ListenPort = 179
	}
	if len(out.Runtime.Global.Families) == 0 {
		out.Runtime.Global.Families = []string{"ipv4-unicast", "ipv6-unicast"}
	}
	if out.Table == 0 {
		out.Table = 254
	}
	if out.RouteMetric == 0 {
		out.RouteMetric = 200
	}
	if out.MaxRoutes == 0 {
		out.MaxRoutes = 100000
	}
	if out.EffectTimeoutMilliseconds == 0 {
		out.EffectTimeoutMilliseconds = 2000
	}
	for peer, ref := range out.PeerBFD {
		if ref.Address == "" {
			ref.Address = peer
			out.PeerBFD[peer] = ref
		}
	}
	return out
}

func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" || len(c.Resource) > 256 || strings.ContainsAny(c.Resource, "\x00\r\n") {
		return errors.New("BGP resource must be a nonempty bounded identity")
	}
	paths := make(map[string]string)
	for name, path := range map[string]string{"socket_path": c.SocketPath, "grpc_socket_path": c.GRPCSocketPath, "state_path": c.StatePath, "status_path": c.StatusPath, "event_path": c.EventPath} {
		if path == "" && name != "socket_path" && name != "state_path" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsRune(path, 0) {
			return fmt.Errorf("BGP %s must be a clean absolute file path", name)
		}
		if (name == "socket_path" || name == "grpc_socket_path") && len(path) > 100 {
			return fmt.Errorf("BGP %s is too long for a Unix socket", name)
		}
		if name == "grpc_socket_path" && strings.Contains(path, ",") {
			return errors.New("BGP gRPC socket path cannot contain a comma")
		}
		for _, owned := range []string{path, path + ".lock", path + ".1"} {
			if prev, ok := paths[owned]; ok {
				return fmt.Errorf("BGP %s overlaps %s", name, prev)
			}
			paths[owned] = name
		}
	}
	if err := ValidateRuntime(c.Runtime); err != nil {
		return err
	}
	c = Normalize(c)
	if c.Table < 1 || uint64(c.Table) > 4294967295 || c.RouteMetric < 0 || uint64(c.RouteMetric) > 4294967295 {
		return errors.New("BGP routing table and metric must fit unsigned 32-bit values")
	}
	if c.Table == 255 {
		return errors.New("BGP learned routes cannot use the kernel local routing table")
	}
	if c.MaxRoutes < 1 || c.MaxRoutes > 1000000 {
		return errors.New("BGP max_routes must be 1..1000000")
	}
	if c.EffectTimeoutMilliseconds < 10 || c.EffectTimeoutMilliseconds > 4000 {
		return errors.New("BGP effect timeout must be 10..4000 milliseconds")
	}
	if c.FIBInterface != "" && !validInterface(c.FIBInterface) {
		return errors.New("BGP fib_interface is invalid")
	}
	if c.BindInterface != "" && !validInterface(c.BindInterface) {
		return errors.New("BGP bind_interface is invalid")
	}
	for peer, tr := range c.PeerTransports {
		p, ok := c.Runtime.Peers[peer]
		if !ok || p.Address != peer {
			return fmt.Errorf("BGP transport has no configured peer %q", peer)
		}
		if tr.RemotePort < 0 || tr.RemotePort > 65535 {
			return errors.New("BGP remote_port must be 1..65535 or omitted")
		}
		if tr.BindInterface != "" && !validInterface(tr.BindInterface) {
			return errors.New("BGP bind_interface is invalid")
		}
		if tr.FIBInterface != "" && !validInterface(tr.FIBInterface) {
			return errors.New("BGP peer fib_interface is invalid")
		}
		if tr.LocalAddress != "" {
			a, err := netip.ParseAddr(tr.LocalAddress)
			remote, _ := netip.ParseAddr(peer)
			if err != nil || a.Is4In6() || a.Zone() != "" || a.IsMulticast() || a.Is4() != remote.Is4() {
				return errors.New("BGP local_address must be an address of the peer family")
			}
		}
	}
	if err := ValidateBFDReferences(c); err != nil {
		return err
	}
	return nil
}

func validInterface(s string) bool {
	return s != "" && len(s) <= 15 && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00 \t\r\n:")
}

func ValidateRuntime(c bgpdaemon.AppliedConfig) error {
	// Validate values before Normalize drops empty paths or repairs an invalid
	// multihop value. Normalization must not turn invalid intent into success.
	for _, p := range c.Paths {
		if strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Prefix) == "" {
			return errors.New("BGP paths require source and prefix")
		}
	}
	for _, p := range c.Peers {
		if p.EbgpMultihop < 0 || p.EbgpMultihop > 255 {
			return errors.New("BGP ebgpMultihop must be 0..255")
		}
	}
	c = bgpdaemon.Normalize(c)
	if err := bgpdaemon.Validate(c); err != nil {
		return err
	}
	id, err := netip.ParseAddr(c.Global.RouterID)
	if err != nil || !id.Is4() || id.IsUnspecified() || id.IsMulticast() {
		return errors.New("BGP routerID must be a nonzero unicast IPv4 identifier")
	}
	if c.Global.ListenPort < -1 || c.Global.ListenPort > 65535 {
		return errors.New("BGP listenPort must be -1 (disabled), 0 (default), or 1..65535")
	}
	for _, s := range c.Global.ListenAddresses {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" || a.Is4In6() || a.IsMulticast() {
			return errors.New("BGP listenAddresses must contain IP addresses")
		}
	}
	for _, family := range c.Global.Families {
		if family != "ipv4-unicast" && family != "ipv6-unicast" {
			return errors.New("native BGP supports IPv4 and IPv6 unicast families")
		}
	}
	if err := validateImportPolicy(c.Global.ImportPolicy); err != nil {
		return err
	}
	if err := validateGracefulRestart(c.Global.GracefulRestart); err != nil {
		return err
	}
	exports := make(map[string]string)
	for address, p := range c.Peers {
		a, err := netip.ParseAddr(address)
		if err != nil || a.Zone() != "" || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() {
			return fmt.Errorf("invalid BGP peer address %q", address)
		}
		if len(p.Password) > 80 || strings.ContainsRune(p.Password, 0) {
			return errors.New("BGP TCP MD5 password must be at most 80 bytes without NUL")
		}
		if p.BFD != "" {
			return errors.New("legacy BGP BFD profile references require explicit migration to peer_bfd service references")
		}
		if p.TimersProfile != "" && p.TimersProfile != "default" && p.TimersProfile != "fast" && p.TimersProfile != "slow" {
			return errors.New("unknown BGP timersProfile")
		}
		if p.ConvergenceProfile != "" && p.ConvergenceProfile != "default" && p.ConvergenceProfile != "fast" && p.ConvergenceProfile != "stable" {
			return errors.New("unknown BGP convergenceProfile")
		}
		if p.RouteReflectorClient && p.ASN != c.Global.ASN {
			return errors.New("BGP route reflector clients must use the local ASN")
		}
		if err := validateImportPolicy(p.ImportPolicy); err != nil {
			return err
		}
		if err := validateGracefulRestart(p.GracefulRestart); err != nil {
			return err
		}
		if len(p.ExportPolicy.AllowedPrefixes) > 0 && p.ExportPolicyName == "" {
			return errors.New("BGP export allowlists require an exportPolicyName")
		}
		for _, value := range p.ExportPolicy.AllowedPrefixes {
			if _, err := netip.ParsePrefix(value); err != nil {
				return errors.New("invalid BGP export policy prefix")
			}
		}
		if p.ExportPolicyName != "" {
			b, _ := json.Marshal(p.ExportPolicy)
			if prev, ok := exports[p.ExportPolicyName]; ok && prev != string(b) {
				return errors.New("BGP export policy name has conflicting peer definitions")
			}
			exports[p.ExportPolicyName] = string(b)
		}
	}
	for _, path := range append(c.Paths, c.PendingStaticPathRemovals...) {
		p, _ := netip.ParsePrefix(path.Prefix)
		if path.Attrs.NextHop != "" {
			n, _ := netip.ParseAddr(path.Attrs.NextHop)
			if n.Zone() != "" || n.Is4In6() || n.IsMulticast() || n.Is4() != p.Addr().Is4() {
				return errors.New("BGP originated next hop must match its unicast prefix family")
			}
		}
		for _, community := range path.Attrs.Communities {
			parts := strings.Split(community, ":")
			if !standardCommunity.MatchString(community) || len(parts) > 2 {
				return errors.New("BGP originated communities must use decimal or ASN:value notation")
			}
			bits := 32
			if len(parts) == 2 {
				bits = 16
			}
			for _, part := range parts {
				if _, err := strconv.ParseUint(part, 10, bits); err != nil {
					return errors.New("BGP originated community component is out of range")
				}
			}
		}
	}
	return nil
}

var standardCommunity = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)

func validateImportPolicy(p bgpdaemon.AppliedImportPolicy) error {
	if p.NextHopRewrite != "" && p.NextHopRewrite != "peer-address" && p.NextHopRewrite != "unchanged" {
		return errors.New("BGP nextHopRewrite must be peer-address or unchanged")
	}
	for _, value := range p.AllowedPrefixes {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return errors.New("invalid BGP import policy prefix")
		}
		minimum, maximum := prefix.Bits(), prefix.Addr().BitLen()
		if p.AllowedPrefixLengthMin != 0 {
			minimum = p.AllowedPrefixLengthMin
		}
		if p.AllowedPrefixLengthMax != 0 {
			maximum = p.AllowedPrefixLengthMax
		}
		if minimum < prefix.Bits() || maximum < minimum || maximum > prefix.Addr().BitLen() {
			return errors.New("BGP import prefix length bounds are inconsistent")
		}
	}
	if p.AllowedPrefixLengthMin < 0 || p.AllowedPrefixLengthMax < 0 || p.AllowedPrefixLengthMin > 128 || p.AllowedPrefixLengthMax > 128 {
		return errors.New("BGP import prefix length bounds must be 0..128")
	}
	for _, community := range append(p.RequiredCommunities, p.ForbiddenCommunities...) {
		if len(community) > 256 {
			return errors.New("BGP community match expression is too long")
		}
		if _, err := regexp.Compile(community); err != nil {
			return errors.New("invalid BGP community match expression")
		}
	}
	return nil
}

func validateGracefulRestart(gr *bgpdaemon.AppliedGracefulRestart) error {
	if gr != nil && gr.Enabled && (gr.RestartTime > 4095 || gr.StaleRoutesTime > 604800) {
		return errors.New("BGP graceful restart times exceed supported bounds")
	}
	return nil
}
