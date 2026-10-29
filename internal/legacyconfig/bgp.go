// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Policy inheritance and advertisements follow routerd/controller/bgp at
// SourceRevision. Its redistribute allowlists are static advertisements;
// they are not snapshots of the kernel's currently connected routes.
func (c *compiler) bgp() []resourceCondition {
	var conditions []resourceCondition
	used := map[string]bool{}
	for _, r := range c.resourcesOfKind("BGPRouter") {
		c.allow(r, "asn", "routerID", "vrf", "listen", "importPolicy", "exportPolicy", "redistribute", "communities", "convergenceProfile", "timers", "gracefulRestart", "backend")
		s := r.Spec.(api.BGPRouterSpec)
		if s.Backend != "" && s.Backend != "gobgp" {
			c.reject(r, "backend", "unsupported_semantics", "BGP uses the bundled GoBGP worker")
		}
		if len(s.Communities.Accept) != 0 || len(s.Communities.Set.In) != 0 || s.Communities.Send != "" && s.Communities.Send != "both" {
			c.reject(r, "communities", "unsupported_semantics", "community import transformations require explicit native policy; the worker sends both community families")
		}
		name := c.serviceName(r, "bgp")
		global := bgpdaemon.AppliedGlobal{ASN: s.ASN, RouterID: s.RouterID, ListenPort: 179, Families: []string{"ipv4-unicast"}, UseMultiplePaths: true, ImportPolicy: bgpImport(s.ImportPolicy), GracefulRestart: c.bgpRestart(r, s.GracefulRestart, s.ConvergenceProfile)}
		if s.Listen.Port > 0 {
			global.ListenPort = s.Listen.Port
		}
		if s.Listen.Address != "" {
			global.ListenAddresses = []string{s.Listen.Address}
		}
		intent := serviceconfig.BGP{Runtime: bgpdaemon.AppliedConfig{Global: global, Peers: map[string]bgpdaemon.AppliedPeer{}}}
		if s.VRF != "" {
			vrf := c.getRef(r, "vrf", s.VRF, "VRF")
			if vrf != nil {
				intent.BindInterface = c.aliases["VRF/"+vrf.Metadata.Name]
				intent.Table = vrf.Spec.(api.VRFSpec).RouteTable
			}
		}
		var prefixes []string
		for _, group := range [][]string{s.ExportPolicy.AllowedPrefixes, s.Redistribute.Connected.AllowedPrefixes, s.Redistribute.Static.AllowedPrefixes} {
			for _, value := range group {
				prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
				if err != nil {
					c.reject(r, "exportPolicy", "invalid_value", "BGP advertisement must be an IP prefix")
					continue
				}
				value = prefix.Masked().String()
				if !slices.Contains(prefixes, value) {
					prefixes = append(prefixes, value)
				}
			}
		}
		slices.Sort(prefixes)
		for _, prefix := range prefixes {
			intent.Runtime.Paths = append(intent.Runtime.Paths, bgpdaemon.AppliedPath{Source: bgpdaemon.AppliedPathSourceStatic, Prefix: prefix, Attrs: bgpdaemon.AppliedPathAttrs{Communities: s.Communities.Set.Out}})
		}
		ipv6 := false
		for _, prefix := range append(slices.Clone(prefixes), s.ImportPolicy.AllowedPrefixes...) {
			if p, err := netip.ParsePrefix(prefix); err == nil && p.Addr().Is6() {
				ipv6 = true
			}
		}
		for _, peer := range c.resourcesOfKind("BGPPeer") {
			ps := peer.Spec.(api.BGPPeerSpec)
			if strings.TrimPrefix(ps.RouterRef, "BGPRouter/") != r.Metadata.Name {
				continue
			}
			used[peer.ID()] = true
			c.allow(peer, "routerRef", "peerASN", "peers", "passiveMode", "password", "passwordFrom", "ebgpMultihop", "routeReflectorClient", "routeReflectorClusterID", "importPolicy", "exportPolicy", "timers", "convergenceProfile", "communities", "bfd")
			if ps.PasswordFrom.File != "" || ps.PasswordFrom.Env != "" || ps.PasswordFrom.Base64 {
				c.reject(peer, "passwordFrom", "unsupported_semantics", "external BGP secret references must be supplied through the native secret boundary")
			}
			if ps.BFD != "" {
				c.reject(peer, "bfd", "unsupported_semantics", "BFD requires a declared native fenced BFD provider")
			}
			if ps.Communities.Send != "" || len(ps.Communities.Accept) != 0 || len(ps.Communities.Set.In) != 0 || len(ps.Communities.Set.Out) != 0 {
				c.reject(peer, "communities", "unsupported_semantics", "per-peer community transformations need native policy")
			}
			policy := ps.ImportPolicy
			if len(policy.AllowedPrefixes) == 0 && len(policy.RequiredCommunities) == 0 && len(policy.ForbiddenCommunities) == 0 && policy.NextHopRewrite == "" && policy.LocalPreference == 0 {
				policy = s.ImportPolicy
			}
			export := slices.Clone(prefixes)
			export = append(export, ps.ExportPolicy.AllowedPrefixes...)
			if ps.RouteReflectorClient {
				export = append(export, s.ImportPolicy.AllowedPrefixes...)
			}
			profile := first(ps.ConvergenceProfile, s.ConvergenceProfile)
			for _, address := range ps.Peers {
				parsed, err := netip.ParseAddr(strings.TrimSpace(address))
				if err != nil {
					c.reject(peer, "peers", "invalid_value", "BGP peer must be an IP address")
					continue
				}
				address = parsed.String()
				ipv6 = ipv6 || parsed.Is6()
				if _, exists := intent.Runtime.Peers[address]; exists {
					c.reject(peer, "peers", "conflict", "BGP peer address is declared more than once")
					continue
				}
				intent.Runtime.Peers[address] = bgpdaemon.AppliedPeer{Address: address, ASN: ps.PeerASN, PassiveMode: ps.PassiveMode, Password: ps.Password, EbgpMultihop: ps.EbgpMultihop, RouteReflectorClient: ps.RouteReflectorClient, RouteReflectorClusterID: ps.RouteReflectorClusterID, TimersProfile: first(ps.Timers.Profile, s.Timers.Profile), ConvergenceProfile: profile, ImportPolicy: bgpImport(policy), ExportPolicyName: name + "-" + strings.NewReplacer(".", "-", ":", "-").Replace(address), ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: export}, GracefulRestart: c.bgpRestart(r, s.GracefulRestart, profile)}
			}
			c.target(peer, "services/"+name+"/bgp/peers")
		}
		if ipv6 {
			intent.Runtime.Global.Families = append(intent.Runtime.Global.Families, "ipv6-unicast")
		}
		c.addService(r, serviceconfig.Service{Name: name, BGP: &intent})
		conditions = append(conditions, resourceCondition{id: "service/" + name, source: r, when: s.When})
		c.note(r, "routerd BGP import/export inheritance, static advertised-prefix union, peers and graceful restart feed the existing isolated GoBGP worker and sole FIB writer. No FRR daemon is deployed; FRR remains an independent interoperability peer.")
	}
	for _, r := range c.resourcesOfKind("BGPPeer") {
		if !used[r.ID()] {
			c.reject(r, "routerRef", "reference", "BGP peer must reference a configured BGPRouter")
		}
	}
	return conditions
}

func bgpImport(s api.BGPImportPolicySpec) bgpdaemon.AppliedImportPolicy {
	return bgpdaemon.AppliedImportPolicy{AllowedPrefixes: s.AllowedPrefixes, AllowedPrefixLengthMin: s.AllowedPrefixLengthMin, AllowedPrefixLengthMax: s.AllowedPrefixLengthMax, RequiredCommunities: s.RequiredCommunities, ForbiddenCommunities: s.ForbiddenCommunities, NextHopRewrite: first(s.NextHopRewrite, "peer-address"), LocalPreference: s.LocalPreference}
}

func (c *compiler) bgpRestart(r resource, s api.BGPGracefulRestartSpec, profile string) *bgpdaemon.AppliedGracefulRestart {
	enabled := profile != "fast"
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	if !enabled {
		return nil
	}
	seconds := func(value string, fallback uint32) uint32 {
		if value == "" {
			return fallback
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 0 || duration%time.Second != 0 || duration/time.Second > 65535 {
			c.reject(r, "gracefulRestart", "invalid_value", "BGP restart durations must be whole seconds within uint16")
			return 0
		}
		return uint32(duration / time.Second)
	}
	return &bgpdaemon.AppliedGracefulRestart{Enabled: true, RestartTime: seconds(s.RestartTime, 120), StaleRoutesTime: seconds(s.StalePathTime, 360)}
}
