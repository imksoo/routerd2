// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	bgpstate "github.com/imksoo/routerd/pkg/bgp"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd/pkg/mobilityconfig"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// Compile the configured static transport topology once. Runtime enrollment,
// peer-group publication and remote learned endpoints require their own owners;
// none are replaced with a guessed snapshot here.
func (c *compiler) ipipTransports() ([]resourceprogram.Resource, map[string][]string) {
	var out []resourceprogram.Resource
	bgpDependencies := make(map[string][]string)
	for _, nodes := range c.resourcesOfKind("SAMNodeSet") {
		c.allow(nodes, "nodes")
		c.note(nodes, "Static transport node identity and endpoints are compiled into native IPIP/BGP intent; this does not implement enrollment or mobility ownership.")
	}
	for _, r := range c.resourcesOfKind("SAMTransportProfile") {
		c.allow(r, "selfNodeRef", "mode", "encryption", "innerPrefix", "addressingMode", "underlayInterface", "localEndpointFrom", "bgp", "peersFrom", "publishPeerGroup")
		s := r.Spec.(api.SAMTransportProfileSpec)
		self := strings.TrimSpace(s.SelfNodeRef)
		if self == "" {
			c.reject(r, "selfNodeRef", "required", "transport requires its configured node identity")
			continue
		}
		if s.Mode != "ipip" || s.Encryption != "" && s.Encryption != "none" || s.AddressingMode != "pair-stable" || s.PublishPeerGroup {
			c.reject(r, "mode", "unsupported_semantics", "native static IPIP transport requires pair-stable addressing; encryption and peer-group publication need their own configured owners")
			continue
		}
		inner, err := netip.ParsePrefix(strings.TrimSpace(s.InnerPrefix))
		if err != nil || !inner.Addr().Is4() || inner.Bits() > 31 {
			c.reject(r, "innerPrefix", "invalid_value", "pair-stable transport requires an IPv4 prefix with /31 slots")
			continue
		}
		inner = inner.Masked()
		source := c.getRef(r, "localEndpointFrom.resource", s.LocalEndpointFrom.Resource, "DHCPv4Client")
		if source == nil {
			continue
		}
		if s.LocalEndpointFrom.Field != "currentAddress" {
			c.reject(r, "localEndpointFrom.field", "unsupported_semantics", "IPIP local endpoint follows the DHCPv4 client's currentAddress and original lease")
			continue
		}
		parent := c.interfaceName(r, "underlayInterface", s.UnderlayInterface, true)
		if parent != c.aliases[source.Spec.(api.DHCPv4ClientSpec).Interface] {
			c.reject(r, "underlayInterface", "conflict", "IPIP underlay must be the selected DHCPv4 client's interface")
			continue
		}
		router := c.getRef(r, "bgp.routerRef", s.BGP.RouterRef, "BGPRouter")
		if router == nil {
			continue
		}
		serviceName := c.serviceName(*router, "bgp")
		serviceIndex := -1
		for i := range c.document.Services {
			if c.document.Services[i].Name == serviceName {
				serviceIndex = i
				break
			}
		}
		if serviceIndex < 0 {
			continue
		}
		if s.BGP.BFD.Enabled || s.BGP.DirectLocalPreference != 0 || s.BGP.GeneratePeers != nil && !*s.BGP.GeneratePeers {
			c.reject(r, "bgp", "unsupported_semantics", "this transport path requires BGP peers; BFD and direct enrollment policies need their existing configured providers")
			continue
		}
		nodes := make(map[string]netip.Addr)
		topology := make(map[string]bool)
		for _, from := range s.PeersFrom {
			set := c.getRef(r, "peersFrom.resource", from.Resource, "SAMNodeSet")
			if set == nil {
				continue
			}
			if from.Direct {
				c.reject(r, "peersFrom", "unsupported_semantics", "direct enrollment groups require signed ownership and lifetime input")
				continue
			}
			for _, node := range set.Spec.(api.SAMNodeSetSpec).Nodes {
				name := strings.TrimSpace(node.NodeRef)
				topology[name] = true
				if name == self || len(from.NodeRefs) != 0 && !slices.Contains(from.NodeRefs, name) {
					continue
				}
				if node.SAMEndpointFrom.Resource != "" {
					c.reject(r, "peersFrom", "unsupported_semantics", "remote learned endpoints require their source's lifetime owner")
					continue
				}
				endpoint, err := netip.ParseAddr(node.SAMEndpoint)
				endpoint = endpoint.Unmap()
				if name == "" || err != nil || !endpoint.Is4() || endpoint.IsUnspecified() || endpoint.IsMulticast() {
					c.reject(r, "peersFrom", "invalid_value", "static transport peers need a node identity and IPv4 endpoint")
					continue
				}
				if previous, exists := nodes[name]; exists && previous != endpoint {
					c.reject(r, "peersFrom", "conflict", "a transport node has conflicting endpoints")
					continue
				}
				nodes[name] = endpoint
			}
		}
		names := make([]string, 0, len(nodes))
		for name := range nodes {
			names = append(names, name)
		}
		slices.Sort(names)
		used := make(map[int]string)
		for _, peer := range names {
			slot := mobilityconfig.StableSAMTransportSlot(inner.String(), self, peer, 1<<(31-inner.Bits()))
			if _, exists := used[slot]; exists {
				c.reject(r, "innerPrefix", "conflict", "pair-stable /31 slots collide; expand the configured prefix")
				continue
			}
			used[slot] = peer
			slotPrefix, err := mobilityconfig.SAMTransportSlotPrefix(inner, slot)
			if err != nil {
				c.reject(r, "innerPrefix", "invalid_value", "pair-stable address calculation failed")
				continue
			}
			local, remote := slotPrefix.Addr(), slotPrefix.Addr().Next()
			if self > peer {
				local, remote = remote, local
			}
			sum := sha256.Sum256([]byte(r.Metadata.Name + "\x00" + self + "\x00" + peer))
			name := fmt.Sprintf("ip%x", sum[:6])
			id := "ipip/" + name
			out = append(out, resourceprogram.Resource{ID: id, IPIP: &resourceprogram.IPIP{Name: name, LocalFromDHCP4: c.serviceName(*source, "dhcp4"), Remote: nodes[peer], Address: netip.PrefixFrom(local, 31)}})
			out = append(out, resourceprogram.Resource{ID: id + "/endpoint", DependsOn: []string{id}, Route: &network.Route{Destination: nodes[peer].String() + "/32", Interface: parent}})
			intent := c.document.Services[serviceIndex].BGP
			address := remote.String()
			if _, exists := intent.Runtime.Peers[address]; exists {
				c.reject(r, "innerPrefix", "conflict", "transport inner peer address overlaps a configured peer")
				continue
			}
			policy := s.BGP.ImportPolicy
			policy.RequiredCommunities = slices.Clone(policy.RequiredCommunities)
			policy.ForbiddenCommunities = slices.Clone(policy.ForbiddenCommunities)
			if s.BGP.RouteReflectorClient {
				policy.AllowedPrefixLengthMin, policy.AllowedPrefixLengthMax = 32, 32
				identity := bgpstate.MobilityNodeIdentityCommunity(peer)
				if !slices.Contains(policy.RequiredCommunities, identity) {
					policy.RequiredCommunities = append(policy.RequiredCommunities, identity)
				}
				for node := range topology {
					if node == peer {
						continue
					}
					identity := bgpstate.MobilityNodeIdentityCommunity(node)
					if identity != "" && !slices.Contains(policy.ForbiddenCommunities, identity) {
						policy.ForbiddenCommunities = append(policy.ForbiddenCommunities, identity)
					}
				}
				slices.Sort(policy.ForbiddenCommunities)
			}

			intent.Runtime.Peers[address] = bgpdaemon.AppliedPeer{Address: address, ASN: s.BGP.PeerASN, PassiveMode: local.Compare(remote) > 0, EbgpMultihop: s.BGP.EbgpMultihop, TimersProfile: first(s.BGP.Timers.Profile, s.BGP.TimersPreset), ConvergenceProfile: first(s.BGP.ConvergenceProfile, router.Spec.(api.BGPRouterSpec).ConvergenceProfile), RouteReflectorClient: s.BGP.RouteReflectorClient, RouteReflectorClusterID: s.BGP.RouteReflectorClusterID, ImportPolicy: bgpImport(policy), ExportPolicyName: name, ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: s.BGP.ExportPolicy.AllowedPrefixes}}
			bgpDependencies["service/"+serviceName] = append(bgpDependencies["service/"+serviceName], id)
			c.target(r, "resources/"+id)
		}
	}
	return out, bgpDependencies
}
