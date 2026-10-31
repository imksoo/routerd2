// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"encoding/base64"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
)

// wireguard runs after ordinary aliases and before interface membership or
// addresses. The pinned BuildInterface joins peers by exact resource name;
// neither a kernel ifname nor a generic kind-qualified alias is that join.
func (c *compiler) wireguard() {
	interfaces := c.resourcesOfKind("WireGuardInterface")
	peers := c.resourcesOfKind("WireGuardPeer")
	if len(interfaces) > network.MaxWireGuardInterfaces {
		c.issue(nil, "$.spec.resources", "resource_limit", "WireGuard migration exceeds the native interface limit")
		return
	}
	byName, byKernel := map[string]int{}, map[string]bool{}
	for _, r := range interfaces {
		c.allow(r, "ifname", "privateKey", "privateKeyFile", "listenPort", "mtu", "selfNodeRef", "peersFrom")
		s := r.Spec.(api.WireGuardInterfaceSpec)
		dynamicSAM := len(s.PeersFrom) == 1 && strings.HasPrefix(strings.TrimSpace(s.PeersFrom[0].Resource), "SAMRRSet/") && matchingSAMTransport(c, s)
		if s.PrivateKey != "" {
			c.reject(r, "privateKey", "unsupported_semantics", "inline private keys require secure provisioning to a versioned regular key file before migration; key values are never copied into native configuration")
		}
		if !wireGuardKeyPath(s.PrivateKeyFile) {
			c.reject(r, "privateKeyFile", "required", "a clean absolute private-key file path is required; missing-key pending state cannot become an active native interface")
		}
		if s.SelfNodeRef != "" && !dynamicSAM {
			c.reject(r, "selfNodeRef", "unsupported_semantics", "SAM identity and enrollment require an explicit native controller; only static peer resources are supported")
		}
		if len(s.PeersFrom) != 0 && !dynamicSAM {
			c.reject(r, "peersFrom", "unsupported_semantics", "SAM identity, enrollment and dynamic peer sources require an explicit native controller; only static peer resources are supported")
		}
		if len(s.PeersFrom) != 0 && dynamicSAM && s.SelfNodeRef == "" {
			c.reject(r, "selfNodeRef", "required", "an enrolled WireGuard interface must name its local SAM node")
		}
		if s.FwMark != 0 || s.Table != 0 {
			c.reject(r, "", "unsupported_semantics", "public legacy WireGuard fwmark/table intent is not supported; no route table or mark is invented during migration")
		}
		if s.ListenPort < 0 || s.ListenPort > 65535 {
			c.reject(r, "listenPort", "invalid_value", "listenPort must be 0..65535; zero retains the original automatic-port behavior")
		}
		if s.MTU != 0 && (s.MTU < 576 || s.MTU > 9216) {
			c.reject(r, "mtu", "invalid_value", "MTU must be omitted or within the original range 576..9216")
		}
		name := first(strings.TrimSpace(s.IfName), r.Metadata.Name)
		if !wireGuardInterfaceName(name) || !wireGuardInterfaceName(r.Metadata.Name) {
			c.reject(r, "ifname", "invalid_value", "WireGuard resource and kernel names must be usable Linux interface names of at most 15 bytes")
			continue
		}
		if owner, exists := c.aliasOwner[r.Metadata.Name]; exists && owner != r.ID() {
			c.reject(r, "", "alias_collision", "WireGuard resource name collides with another declared interface alias")
			continue
		}
		if byKernel[name] {
			c.reject(r, "ifname", "conflict", "more than one WireGuard resource owns the same kernel interface")
			continue
		}
		byKernel[name] = true
		c.aliasOwner[r.Metadata.Name] = r.ID()
		c.aliases[r.Metadata.Name], c.aliases[r.Kind+"/"+r.Metadata.Name] = name, name
		c.upAllowed[name] = true
		c.document.Network.Links = append(c.document.Network.Links, network.Link{Name: name, Kind: "wireguard"})
		c.ensureInterface(name).MTU = s.MTU
		byName[r.Metadata.Name] = len(c.document.Network.WireGuard)
		c.document.Network.WireGuard = append(c.document.Network.WireGuard, network.WireGuard{Interface: name, PrivateKeyFile: s.PrivateKeyFile, GeneratePrivateKey: true, ListenPort: s.ListenPort})
		c.target(r, "network.links/"+name)
		c.target(r, "network.interfaces/"+name)
		c.target(r, "network.wireguard/"+name)
		c.note(r, "Preserves an UP WireGuard link, optional MTU, automatic port when omitted, and first-apply private-key generation only when its file is absent. No AllowedIPs routes or firewall mark are inferred.")
		c.note(r, "Key paths must name worker-owned private regular files through trusted nonsymlink directories; provision the parent directory before apply. First import pins an immutable key snapshot; rotate by committing a new versioned path. Existing foreign links and peers are not adopted.")
	}
	publicKeys, prefixes := map[string]map[string]bool{}, map[string]map[string]bool{}
	counts := map[string]int{}
	for _, r := range peers {
		c.allow(r, "interface", "publicKey", "allowedIPs", "endpoint", "persistentKeepalive", "presharedKey", "presharedKeyFile")
		s := r.Spec.(api.WireGuardPeerSpec)
		index, exists := byName[s.Interface]
		if !exists {
			c.reject(r, "interface", "reference", "peer interface must be the exact metadata.name of a declared static WireGuardInterface")
			continue
		}
		device := &c.document.Network.WireGuard[index]
		if !wireGuardPublicKey(s.PublicKey) {
			c.reject(r, "publicKey", "invalid_value", "publicKey must be a nonzero canonical base64-encoded 32-byte public key")
		}
		if publicKeys[s.Interface] == nil {
			publicKeys[s.Interface], prefixes[s.Interface] = map[string]bool{}, map[string]bool{}
		}
		if publicKeys[s.Interface][s.PublicKey] {
			c.reject(r, "publicKey", "conflict", "duplicate public keys cannot silently merge separate legacy peer resources")
		}
		publicKeys[s.Interface][s.PublicKey] = true
		if s.PresharedKey != "" {
			c.reject(r, "presharedKey", "unsupported_semantics", "inline preshared keys require secure provisioning to a versioned regular key file before migration; key values are never copied into native configuration")
		}
		if s.PresharedKeyFile != "" && !wireGuardKeyPath(s.PresharedKeyFile) {
			c.reject(r, "presharedKeyFile", "invalid_value", "presharedKeyFile must be a clean absolute private regular-file path")
		}
		if s.PersistentKeepalive < 0 || s.PersistentKeepalive > 65535 {
			c.reject(r, "persistentKeepalive", "invalid_value", "persistentKeepalive must be 0..65535 seconds; zero disables keepalive")
		}
		peer := network.WireGuardPeer{PublicKey: s.PublicKey, PresharedKeyFile: s.PresharedKeyFile, PersistentKeepalive: s.PersistentKeepalive}
		if s.Endpoint != "" {
			endpoint, err := netip.ParseAddrPort(s.Endpoint)
			addr := endpoint.Addr()
			if err != nil || endpoint.Port() == 0 || addr.Is4In6() || addr.IsUnspecified() || addr.IsMulticast() || addr.Zone() != "" && (!addr.Is6() || !wireGuardInterfaceName(addr.Zone())) || addr.Is6() && addr.IsLinkLocalUnicast() && addr.Zone() == "" {
				c.reject(r, "endpoint", "unsupported_semantics", "endpoint must be a numeric unicast IP and port, with an interface zone for IPv6 link-local; hostname resolution/refresh is not a static migration")
			} else {
				peer.Endpoint = endpoint.String()
			}
		}
		if len(s.AllowedIPs) == 0 {
			c.reject(r, "allowedIPs", "required", "the original peer contract requires at least one allowed IP prefix")
		}
		for i, value := range s.AllowedIPs {
			prefix, err := netip.ParsePrefix(value)
			field := "allowedIPs[" + strconv.Itoa(i) + "]"
			if err != nil || prefix.Addr().Is4In6() {
				c.reject(r, field, "invalid_value", "allowed IP must be an IPv4 or IPv6 prefix")
				continue
			}
			canonical := prefix.Masked().String()
			if prefixes[s.Interface][canonical] {
				c.reject(r, field, "conflict", "the same allowed prefix cannot be assigned twice; legacy peer ordering must not choose an implicit owner")
			}
			prefixes[s.Interface][canonical] = true
			peer.AllowedIPs = append(peer.AllowedIPs, canonical)
		}
		counts[s.Interface] += len(s.AllowedIPs)
		if len(device.Peers) >= network.MaxWireGuardPeers || counts[s.Interface] > network.MaxWireGuardAllowedIPs {
			c.reject(r, "allowedIPs", "resource_limit", "WireGuard peer set exceeds the native peer or allowed-prefix limit")
			continue
		}
		sort.Strings(peer.AllowedIPs)
		device.Peers = append(device.Peers, peer)
		c.target(r, "network.wireguard/"+device.Interface+"/peers/"+s.PublicKey)
		c.note(r, "AllowedIPs retains peer selection/source validation only; explicit address, route and forwarding declarations remain separate. An omitted endpoint remains passive and authenticated endpoint roaming is retained.")
	}
	for i := range c.document.Network.WireGuard {
		device := &c.document.Network.WireGuard[i]
		sort.Slice(device.Peers, func(i, j int) bool { return device.Peers[i].PublicKey < device.Peers[j].PublicKey })
	}
}

func matchingSAMTransport(c *compiler, wg api.WireGuardInterfaceSpec) bool {
	self := strings.TrimSpace(wg.SelfNodeRef)
	if self == "" || len(wg.PeersFrom) != 1 {
		return false
	}
	rrSet := strings.TrimSpace(wg.PeersFrom[0].Resource)
	for _, profile := range c.resourcesOfKind("SAMTransportProfile") {
		spec := profile.Spec.(api.SAMTransportProfileSpec)
		if strings.TrimSpace(spec.SelfNodeRef) == self && strings.EqualFold(strings.TrimSpace(spec.Encryption), "wireguard") && profileUsesRRSet(spec, rrSet) {
			return true
		}
	}
	return false
}

func wireGuardKeyPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) <= 4096 && !strings.ContainsAny(path, "\x00\r\n")
}

func wireGuardInterfaceName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 15 {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func wireGuardPublicKey(value string) bool {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != 32 || base64.StdEncoding.EncodeToString(decoded) != value {
		return false
	}
	for _, b := range decoded {
		if b != 0 {
			return true
		}
	}
	return false
}
