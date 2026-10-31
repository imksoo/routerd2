// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// samTransports keeps the configured half of a leaf's RR transport separate
// from the RR-provided, expiring topology. The runtime joins them only after a
// verified sam_client observation arrives.
func (c *compiler) samTransports() []resourceprogram.Resource {
	var out []resourceprogram.Resource
	used := make(map[string]bool)
	for _, client := range c.resourcesOfKind("SAMEnrollmentClient") {
		clientSpec := client.Spec.(api.SAMEnrollmentClientSpec)
		claim := c.getRef(client, "claimRef", clientSpec.ClaimRef, "SAMEnrollmentClaim")
		if claim == nil {
			continue
		}
		claimSpec := claim.Spec.(api.SAMEnrollmentClaimSpec)
		var profiles []resource
		for _, profile := range c.resourcesOfKind("SAMTransportProfile") {
			s := profile.Spec.(api.SAMTransportProfileSpec)
			if strings.TrimSpace(s.SelfNodeRef) != strings.TrimSpace(claimSpec.LeafID) || !profileUsesRRSet(s, claimSpec.RRSetRef) {
				continue
			}
			profiles = append(profiles, profile)
		}
		if len(profiles) == 0 {
			continue
		}
		if len(profiles) != 1 {
			c.reject(client, "claimRef", "ambiguous_transport", "one enrollment claim may select only one native RR transport profile")
			continue
		}
		profile := profiles[0]
		used[profile.ID()] = true
		s := profile.Spec.(api.SAMTransportProfileSpec)
		if claimSpec.DirectMesh {
			c.reject(client, "claimRef", "unsupported_semantics", "direct-leaf SAM topology needs its own bounded BGP policy; this native transport currently uses the RR fallback only")
			continue
		}
		if s.Mode != "ipip" || s.Encryption != "" && s.Encryption != "none" || s.AddressingMode != "pair-stable" || s.PublishPeerGroup {
			c.reject(profile, "mode", "unsupported_semantics", "native enrollment transport currently requires unencrypted pair-stable IPIP without peer-group publication")
			continue
		}
		if s.BGP.GeneratePeers != nil && !*s.BGP.GeneratePeers || s.BGP.BFD.Enabled || s.BGP.RouteReflectorClient || s.BGP.DirectLocalPreference != 0 {
			c.reject(profile, "bgp", "unsupported_semantics", "native leaf RR transport requires generated non-BFD peers; RR-client and direct-leaf policies have separate owners")
			continue
		}
		if !onlyRRSetSource(s, claimSpec.RRSetRef) {
			c.reject(profile, "peersFrom", "unsupported_semantics", "native leaf RR transport currently accepts only its claim's non-direct RR set")
			continue
		}
		inner, err := netip.ParsePrefix(strings.TrimSpace(s.InnerPrefix))
		if err != nil || !inner.Addr().Is4() || inner.Bits() > 31 {
			c.reject(profile, "innerPrefix", "invalid_value", "pair-stable enrollment transport requires an IPv4 prefix with /31 slots")
			continue
		}
		inner = inner.Masked()
		underlay := c.interfaceName(profile, "underlayInterface", s.UnderlayInterface, true)
		if underlay == "" {
			continue
		}
		local, dhcp := "", ""
		if s.LocalEndpoint != "" && s.LocalEndpointFrom.Resource != "" {
			c.reject(profile, "localEndpoint", "conflict", "native enrollment transport requires one local endpoint source")
			continue
		}
		if s.LocalEndpoint != "" {
			address, err := netip.ParseAddr(strings.TrimSpace(s.LocalEndpoint))
			if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
				c.reject(profile, "localEndpoint", "invalid_value", "IPIP enrollment transport requires a unicast IPv4 local endpoint")
				continue
			}
			local = address.String()
		} else {
			source := c.getRef(profile, "localEndpointFrom.resource", s.LocalEndpointFrom.Resource, "DHCPv4Client")
			if source == nil {
				continue
			}
			if s.LocalEndpointFrom.Field != "currentAddress" || c.aliases[source.Spec.(api.DHCPv4ClientSpec).Interface] != underlay {
				c.reject(profile, "localEndpointFrom", "unsupported_semantics", "IPIP enrollment transport follows the selected DHCPv4 client's currentAddress on its underlay")
				continue
			}
			dhcp = c.serviceName(*source, "dhcp4")
		}
		router := c.getRef(profile, "bgp.routerRef", s.BGP.RouterRef, "BGPRouter")
		if router == nil {
			continue
		}
		bgp := c.serviceName(*router, "bgp")
		found := false
		for _, service := range c.document.Services {
			found = found || service.Name == bgp && service.BGP != nil
		}
		if !found {
			continue
		}
		transport := resourceprogram.SAMTransport{
			Name:               profile.Metadata.Name,
			Client:             "service." + c.serviceName(client, "sam"),
			ClaimRef:           "SAMEnrollmentClaim/" + claim.Metadata.Name,
			PolicyRef:          claimSpec.PolicyRef,
			RRSetRef:           claimSpec.RRSetRef,
			Self:               claimSpec.LeafID,
			InnerPrefix:        inner,
			Underlay:           underlay,
			LocalEndpoint:      local,
			LocalFromDHCP4:     prefixedService(dhcp),
			BGP:                "service." + bgp,
			PeerASN:            s.BGP.PeerASN,
			EbgpMultihop:       s.BGP.EbgpMultihop,
			TimersProfile:      first(s.BGP.Timers.Profile, s.BGP.TimersPreset),
			ConvergenceProfile: first(s.BGP.ConvergenceProfile, router.Spec.(api.BGPRouterSpec).ConvergenceProfile),
			ImportPolicy:       bgpImport(s.BGP.ImportPolicy),
			ExportPolicy:       bgpdaemon.AppliedExportPolicy{AllowedPrefixes: s.BGP.ExportPolicy.AllowedPrefixes},
		}
		out = append(out, resourceprogram.Resource{ID: "sam_transport/" + profile.Metadata.Name, SAMTransport: &transport})
		c.target(profile, "resources/sam_transport/"+profile.Metadata.Name)
		c.target(client, "resources/sam_transport/"+profile.Metadata.Name)
		c.note(profile, "The configured local IPIP/BGP recipe is joined at runtime only with this enrollment client's admitted RR topology and original deadline. Direct peer groups are not imported by this RR fallback path.")
	}
	for _, profile := range c.resourcesOfKind("SAMTransportProfile") {
		s := profile.Spec.(api.SAMTransportProfileSpec)
		if profileUsesDynamicSource(s) && !used[profile.ID()] {
			c.reject(profile, "peersFrom", "unsupported_semantics", "a learned SAM peer source needs one matching native SAMEnrollmentClient claim and RR set")
		}
	}
	return out
}

func profileUsesRRSet(profile api.SAMTransportProfileSpec, rrSet string) bool {
	for _, source := range profile.PeersFrom {
		if source.Direct || strings.TrimSpace(source.Resource) != strings.TrimSpace(rrSet) {
			continue
		}
		return true
	}
	return false
}

func profileUsesDynamicSource(profile api.SAMTransportProfileSpec) bool {
	for _, source := range profile.PeersFrom {
		kind, _, ok := strings.Cut(strings.TrimSpace(source.Resource), "/")
		if ok && (kind == "SAMRRSet" || kind == "SAMEnrollmentPolicy" || kind == "SAMPeerGroup") {
			return true
		}
	}
	return false
}

func onlyRRSetSource(profile api.SAMTransportProfileSpec, rrSet string) bool {
	return len(profile.PeersFrom) == 1 && !profile.PeersFrom[0].Direct && strings.TrimSpace(profile.PeersFrom[0].Resource) == strings.TrimSpace(rrSet)
}

func prefixedService(name string) string {
	if name == "" {
		return ""
	}
	return "service." + name
}
