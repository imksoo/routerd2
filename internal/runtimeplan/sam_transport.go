package runtimeplan

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd/pkg/mobilityconfig"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// projectSAMTransports lowers only the admitted RR fallback. The recipe is
// operator intent; the RR observation contributes remote endpoints and its
// original finite authority. Direct leaf groups have a different route-policy
// boundary and are deliberately not consumed here.
func projectSAMTransports(p *projection, recipes []samTransportPlan, records map[string]serviceconfig.Update, dhcp4Addresses map[string]leasedIPv4, clock boottime.Clock, configs map[string]bgpconfig.Config) error {
	dynamicBGP := make(map[string]bool)
	for _, recipe := range recipes {
		record, ok := records[recipe.Client]
		if !ok || record.Role != "sam_client" || !matchesBinding(p.instances, recipe.Client, record) {
			continue
		}
		var snapshot samclientconfig.Snapshot
		if err := json.Unmarshal(record.Snapshot, &snapshot); err != nil {
			return err
		}
		if !currentSAMRR(recipe.SAMTransport, snapshot, clock) {
			continue
		}
		local, source, until, ok := samLocalEndpoint(recipe.SAMTransport, dhcp4Addresses, clock, snapshot)
		if !ok {
			continue
		}
		peers, err := samRRPeers(recipe.SAMTransport, snapshot, local)
		if err != nil {
			return fmt.Errorf("SAM transport %s: %w", recipe.Name, err)
		}
		if len(peers) == 0 {
			continue
		}
		var wireguard *network.WireGuard
		if recipe.WireGuardInterface != "" {
			for i := range p.network.WireGuard {
				if p.network.WireGuard[i].Interface == recipe.WireGuardInterface {
					wireguard = &p.network.WireGuard[i]
					break
				}
			}
			if wireguard == nil {
				return fmt.Errorf("SAM transport %s has no WireGuard device %s", recipe.Name, recipe.WireGuardInterface)
			}
			for _, peer := range peers {
				found := false
				for _, configured := range wireguard.Peers {
					found = found || configured.PublicKey == peer.wireguard.PublicKey
				}
				if !found {
					wireguard.Peers = append(wireguard.Peers, peer.wireguard)
				}
			}
		}
		cfg, ok := configs[recipe.BGP]
		if !ok {
			return fmt.Errorf("SAM transport %s has no BGP configuration", recipe.Name)
		}
		if cfg.Runtime.Peers == nil {
			cfg.Runtime.Peers = make(map[string]bgpdaemon.AppliedPeer)
		}
		cfg.Runtime.Paths = append(cfg.Runtime.Paths, recipe.paths...)
		for _, peer := range peers {
			outer, ok := outerIPv4Route(p.network, peer.endpoint.String(), recipe.Underlay, source, clock, until)
			if !ok {
				return fmt.Errorf("SAM transport %s has no current underlay route to RR endpoint %s", recipe.Name, peer.endpoint)
			}
			if _, exists := cfg.Runtime.Peers[peer.remote.String()]; exists {
				return fmt.Errorf("SAM transport %s inner BGP peer %s conflicts with configured peer", recipe.Name, peer.remote)
			}
			cfg.Runtime.Peers[peer.remote.String()] = bgpdaemon.AppliedPeer{
				Address:            peer.remote.String(),
				ASN:                recipe.PeerASN,
				PassiveMode:        peer.local.Compare(peer.remote) > 0,
				EbgpMultihop:       recipe.EbgpMultihop,
				TimersProfile:      recipe.TimersProfile,
				ConvergenceProfile: recipe.ConvergenceProfile,
				ImportPolicy:       recipe.ImportPolicy,
				ExportPolicyName:   "sam-" + peer.link,
				ExportPolicy:       recipe.ExportPolicy,
			}
			p.network.Links = append(p.network.Links, network.Link{Name: peer.link, Kind: "ipip", Parent: recipe.Underlay, Local: local.String(), Remote: peer.endpoint.String(), TTL: 64, Authority: until})
			inheritSAMForwarding(&p.network, recipe.Underlay, peer.link)
			if err := addAddressWithOptions(&p.network, peer.link, netip.PrefixFrom(peer.local, 31).String(), network.AddressOptions{BootLifetime: network.BootLifetime{BootID: until.BootID, PreferredNanos: until.Nanos, ValidNanos: until.Nanos}}); err != nil {
				return err
			}
			if outer.Destination != "" {
				p.network.Routes = append(p.network.Routes, outer)
			}
		}
		configs[recipe.BGP] = bgpconfig.Normalize(cfg)
		dynamicBGP[recipe.BGP] = true
	}
	for module := range dynamicBGP {
		cfg := configs[module]
		if err := bgpconfig.Validate(cfg); err != nil {
			return fmt.Errorf("SAM transport BGP %s: %w", module, err)
		}
		if err := replaceBGPBinding(p, module, cfg); err != nil {
			return err
		}
	}
	return nil
}

type samTransportPlan struct {
	resourceprogram.SAMTransport
	paths []bgpdaemon.AppliedPath
}

func prepareSAMTransports(recipes []resourceprogram.SAMTransport, services []serviceconfig.Service) ([]samTransportPlan, error) {
	plans := make([]samTransportPlan, 0, len(recipes))
	for _, recipe := range recipes {
		var client *samclientconfig.Node
		for _, service := range services {
			if service.ModuleName() == recipe.Client {
				client = service.SAMClient
				break
			}
		}
		if client == nil {
			return nil, fmt.Errorf("SAM transport %s has no enrollment client %s", recipe.Name, recipe.Client)
		}
		paths, err := samOwnedPaths(recipe, client.Claim.Mobility.OwnedAddresses)
		if err != nil {
			return nil, fmt.Errorf("SAM transport %s ownership: %w", recipe.Name, err)
		}
		plans = append(plans, samTransportPlan{SAMTransport: recipe, paths: paths})
	}
	return plans, nil
}

func samOwnedPaths(recipe resourceprogram.SAMTransport, values []string) ([]bgpdaemon.AppliedPath, error) {
	paths := make([]bgpdaemon.AppliedPath, 0, len(values))
	seen := make(map[string]bool, len(values))
	source := "MobilityPool/" + recipe.Name + "/node/" + recipe.Self
	for _, raw := range values {
		prefix, err := samenrollment.ParsePrefixOrAddress(raw)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 {
			return nil, fmt.Errorf("owned address %q is not an IPv4 /32", raw)
		}
		if seen[prefix.String()] {
			return nil, fmt.Errorf("owned address %s is repeated", prefix)
		}
		seen[prefix.String()] = true
		paths = append(paths, bgpdaemon.AppliedPath{Source: source, Prefix: prefix.String(), Family: bgpdaemon.AppliedPathFamilyIPv4Unicast})
	}
	return paths, nil
}

func inheritSAMForwarding(desired *network.Desired, parent, child string) {
	firewall := desired.Firewall
	if firewall == nil {
		return
	}
	for _, forward := range firewall.Forward {
		if forward.From == parent {
			firewall.Forward = append(firewall.Forward, network.Forward{From: child, To: forward.To})
		}
		if forward.To == parent {
			firewall.Forward = append(firewall.Forward, network.Forward{From: forward.From, To: child})
		}
	}
	if firewall.ForwardChain != nil {
		for _, rule := range firewall.ForwardChain.Rules {
			if rule.Action != "accept" {
				continue
			}
			if rule.Match.InInterface == parent {
				inherited := rule
				inherited.Match.InInterface = child
				firewall.ForwardChain.Rules = append(firewall.ForwardChain.Rules, inherited)
			}
			if rule.Match.OutInterface == parent {
				inherited := rule
				inherited.Match.OutInterface = child
				firewall.ForwardChain.Rules = append(firewall.ForwardChain.Rules, inherited)
			}
		}
	}
}

type samRRPeer struct {
	endpoint  netip.Addr
	local     netip.Addr
	remote    netip.Addr
	link      string
	wireguard network.WireGuardPeer
}

func currentSAMRR(recipe resourceprogram.SAMTransport, snapshot samclientconfig.Snapshot, clock boottime.Clock) bool {
	if samclientconfig.ValidateSnapshot(snapshot) != nil || snapshot.RRSet == nil || snapshot.BootID != clock.BootID || snapshot.ExpiresNanos <= uint64(clock.BoottimeNanoseconds) {
		return false
	}
	if snapshot.Phase != "ready" && snapshot.Phase != "degraded" {
		return false
	}
	return snapshot.ClaimRef == recipe.ClaimRef && snapshot.RRSet.EnrollmentPolicyRef == recipe.PolicyRef
}

func samLocalEndpoint(recipe resourceprogram.SAMTransport, addresses map[string]leasedIPv4, clock boottime.Clock, snapshot samclientconfig.Snapshot) (netip.Addr, *leasedIPv4, lifetime.Deadline, bool) {
	until := lifetime.Deadline{BootID: snapshot.BootID, Nanos: snapshot.ExpiresNanos}
	if recipe.LocalFromDHCP4 == "" {
		address, err := netip.ParseAddr(recipe.LocalEndpoint)
		if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
			return netip.Addr{}, nil, lifetime.Deadline{}, false
		}
		return address, nil, until, true
	}
	lease, ok := addresses[recipe.LocalFromDHCP4]
	if !ok || !lease.validUntil.Live(clock) {
		return netip.Addr{}, nil, lifetime.Deadline{}, false
	}
	return lease.address, &lease, lifetime.Minimum(clock, until, lease.validUntil), true
}

func samRRPeers(recipe resourceprogram.SAMTransport, snapshot samclientconfig.Snapshot, local netip.Addr) ([]samRRPeer, error) {
	byName := make(map[string]netip.Addr)
	wireguardByName := make(map[string]network.WireGuardPeer)
	for _, node := range snapshot.RRSet.Nodes {
		name := node.NodeRef
		if name == "" {
			return nil, fmt.Errorf("RR topology has an empty node identity")
		}
		if name == recipe.Self {
			continue
		}
		if recipe.WireGuardInterface == "" && node.SAMEndpointFrom.Resource != "" {
			return nil, fmt.Errorf("RR node %q has an unresolved endpoint source", name)
		}
		endpointText := node.SAMEndpoint
		if recipe.WireGuardInterface != "" {
			endpointText = node.WireGuard.TransportEndpoint
			wireguard, err := samWireGuardPeer(node)
			if err != nil {
				return nil, fmt.Errorf("RR node %q WireGuard peer: %w", name, err)
			}
			wireguardByName[name] = wireguard
		}
		endpoint, err := netip.ParseAddr(endpointText)
		if err != nil {
			return nil, fmt.Errorf("RR node %q endpoint: %w", name, err)
		}
		endpoint = endpoint.Unmap()
		if err := resourceprogram.ValidateSAMTransportPeer(recipe, endpoint); err != nil {
			return nil, fmt.Errorf("RR node %q: %w", name, err)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("RR topology repeats node %q", name)
		}
		byName[name] = endpoint
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	used := make(map[int]string)
	peers := make([]samRRPeer, 0, len(names))
	for _, name := range names {
		slot := mobilityconfig.StableSAMTransportSlot(recipe.InnerPrefix.String(), recipe.Self, name, 1<<(31-recipe.InnerPrefix.Bits()))
		if previous := used[slot]; previous != "" {
			return nil, fmt.Errorf("RR nodes %s and %s collide in pair-stable slot", previous, name)
		}
		used[slot] = name
		prefix, err := mobilityconfig.SAMTransportSlotPrefix(recipe.InnerPrefix, slot)
		if err != nil {
			return nil, err
		}
		left, right := prefix.Addr(), prefix.Addr().Next()
		if recipe.Self > name {
			left, right = right, left
		}
		if local == byName[name] {
			return nil, fmt.Errorf("RR node %q endpoint equals local endpoint", name)
		}
		sum := sha256.Sum256([]byte(recipe.Name + "\x00" + recipe.Self + "\x00" + name))
		peers = append(peers, samRRPeer{endpoint: byName[name], local: left, remote: right, link: fmt.Sprintf("ip%x", sum[:6]), wireguard: wireguardByName[name]})
	}
	return peers, nil
}

func samWireGuardPeer(node api.SAMNodeSpec) (network.WireGuardPeer, error) {
	spec := node.WireGuard
	key, err := base64.StdEncoding.Strict().DecodeString(spec.PublicKey)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != spec.PublicKey {
		return network.WireGuardPeer{}, fmt.Errorf("invalid public key")
	}
	var nonzero byte
	for _, value := range key {
		nonzero |= value
	}
	if nonzero == 0 {
		return network.WireGuardPeer{}, fmt.Errorf("zero public key")
	}
	endpoint, err := netip.ParseAddrPort(spec.Endpoint)
	if err != nil || endpoint.Port() == 0 || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() || endpoint.Addr().Zone() != "" || endpoint.Addr().Is4In6() {
		return network.WireGuardPeer{}, fmt.Errorf("invalid UDP endpoint")
	}
	if spec.PersistentKeepalive < 0 || spec.PersistentKeepalive > 65535 || len(spec.AllowedIPs) > network.MaxWireGuardAllowedIPs {
		return network.WireGuardPeer{}, fmt.Errorf("invalid peer limits")
	}
	peer := network.WireGuardPeer{PublicKey: spec.PublicKey, Endpoint: endpoint.String(), PersistentKeepalive: spec.PersistentKeepalive}
	transport, err := netip.ParseAddr(spec.TransportEndpoint)
	if err != nil || !transport.Is4() || transport.IsUnspecified() || transport.IsMulticast() {
		return network.WireGuardPeer{}, fmt.Errorf("invalid inner transport endpoint")
	}
	var routed bool
	for _, raw := range spec.AllowedIPs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() {
			return network.WireGuardPeer{}, fmt.Errorf("invalid allowed IP %q", raw)
		}
		prefix = prefix.Masked()
		peer.AllowedIPs = append(peer.AllowedIPs, prefix.String())
		routed = routed || prefix.Contains(transport)
	}
	if !routed {
		return network.WireGuardPeer{}, fmt.Errorf("allowed IPs do not include the IPIP transport endpoint")
	}
	return peer, nil
}

func cloneBGPConfigs(source map[string]bgpconfig.Config) map[string]bgpconfig.Config {
	result := make(map[string]bgpconfig.Config, len(source))
	for name, cfg := range source {
		result[name] = bgpconfig.Normalize(cfg)
	}
	return result
}

func replaceBGPBinding(p *projection, module string, cfg bgpconfig.Config) error {
	for i := range p.instances {
		if p.instances[i].instance.Name != module {
			continue
		}
		instance := p.instances[i].instance
		if instance.Role != "bgp" {
			return fmt.Errorf("SAM transport BGP service %s has wrong role", module)
		}
		body, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		instance.Config = body
		bound, err := encodeInstance(instance)
		if err != nil {
			return err
		}
		p.instances[i] = bound
		return nil
	}
	return fmt.Errorf("SAM transport BGP service %s is not active", module)
}

func matchesBinding(bindings []binding, module string, update serviceconfig.Update) bool {
	for _, binding := range bindings {
		if binding.instance.Name == module {
			return binding.instance.Role == update.Role && binding.digest == update.ConfigDigest
		}
	}
	return false
}
