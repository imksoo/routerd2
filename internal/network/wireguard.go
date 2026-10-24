package network

import (
	"context"
	"crypto/ecdh"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// These bounds limit journal size, netlink work and readback memory. The
	// kernel can support larger deployments; raise the bounds deliberately with
	// corresponding load tests rather than sending an unbounded API request.
	MaxWireGuardInterfaces = 64
	MaxWireGuardPeers      = 1024
	MaxWireGuardAllowedIPs = 16384
)

// WireGuard configures a Link of kind wireguard exclusively created by this
// engine. The Linux kernel performs encryption and transports packets. Routes,
// policy rules, addresses, MTU and firewall permissions are separate explicit
// declarations; AllowedIPs never installs FIB routes implicitly.
//
// Key files contain one base64-encoded 32-byte key. They must be regular files
// owned by the worker's effective user and inaccessible to group/other users.
// On first use, a file path is bound to an immutable private snapshot. Rotate
// keys by committing a NEW file path; changing a previously bound source file
// does not change a running or recovered device. This also makes rollback work
// when the original source file has disappeared. No key bytes enter Desired,
// the ownership journal, logs or public status.
type WireGuard struct {
	Interface          string `json:"interface"`
	PrivateKeyFile     string `json:"private_key_file"`
	GeneratePrivateKey bool   `json:"generate_private_key,omitempty"`
	// Zero requests a random port on the first UP transition. Its selected
	// value is retained across reconciliation and updates of the same device.
	ListenPort   int             `json:"listen_port,omitempty"`
	FirewallMark uint32          `json:"firewall_mark,omitempty"`
	Peers        []WireGuardPeer `json:"peers,omitempty"`
}

// WireGuardPeer is part of the complete desired peer set. Endpoint is a numeric
// unicast IP:port; an IPv6 link-local address requires an interface zone. Empty
// means a passive peer, whose endpoint can be learned by authenticated traffic.
// A roaming runtime endpoint is preserved unless this declaration changes.
type WireGuardPeer struct {
	PublicKey           string   `json:"public_key"`
	PresharedKeyFile    string   `json:"preshared_key_file,omitempty"`
	AllowedIPs          []string `json:"allowed_ips,omitempty"`
	Endpoint            string   `json:"endpoint,omitempty"`
	PersistentKeepalive int      `json:"persistent_keepalive,omitempty"`
}

// WireGuardState deliberately contains public data only. Never publish the
// native device readback: the kernel API also returns private keys and PSKs.
type WireGuardState struct {
	Interface    string               `json:"interface"`
	Index        int                  `json:"index"`
	Owned        bool                 `json:"owned"`
	Quarantined  bool                 `json:"quarantined"`
	PublicKey    string               `json:"public_key,omitempty"`
	ListenPort   int                  `json:"listen_port"`
	FirewallMark uint32               `json:"firewall_mark"`
	Peers        []WireGuardPeerState `json:"peers,omitempty"`
}

type WireGuardPeerState struct {
	PublicKey           string    `json:"public_key"`
	AllowedIPs          []string  `json:"allowed_ips,omitempty"`
	Endpoint            string    `json:"endpoint,omitempty"`
	PersistentKeepalive int       `json:"persistent_keepalive"`
	LastHandshake       time.Time `json:"last_handshake,omitzero"`
	ReceivedBytes       uint64    `json:"received_bytes"`
	TransmittedBytes    uint64    `json:"transmitted_bytes"`
}

// wireGuardKey is private memory, never a public/configuration type. Defensive
// formatting prevents a later diagnostic from accidentally exposing its bytes.
type wireGuardKey [32]byte

func (wireGuardKey) String() string               { return "[redacted WireGuard key]" }
func (wireGuardKey) GoString() string             { return "[redacted WireGuard key]" }
func (wireGuardKey) MarshalJSON() ([]byte, error) { return []byte(`"[redacted WireGuard key]"`), nil }
func (key wireGuardKey) encoded() string          { return base64.StdEncoding.EncodeToString(key[:]) }
func (key wireGuardKey) zero() bool               { return key.equal(wireGuardKey{}) }
func (key wireGuardKey) equal(b wireGuardKey) bool {
	return subtle.ConstantTimeCompare(key[:], b[:]) == 1
}

func parseWireGuardKey(value string) (wireGuardKey, error) {
	var key wireGuardKey
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(data) != len(key) || base64.StdEncoding.EncodeToString(data) != value {
		clear(data)
		return key, errors.New("expected a canonical base64-encoded 32-byte WireGuard key")
	}
	copy(key[:], data)
	clear(data)
	return key, nil
}

func (key wireGuardKey) public() (wireGuardKey, error) {
	if key.zero() {
		return wireGuardKey{}, nil
	}
	private, err := ecdh.X25519().NewPrivateKey(key[:])
	if err != nil {
		return wireGuardKey{}, errors.New("invalid WireGuard private key")
	}
	var out wireGuardKey
	copy(out[:], private.PublicKey().Bytes())
	return out, nil
}

type wireGuardDevice struct {
	Index        int
	PrivateKey   wireGuardKey
	PublicKey    wireGuardKey
	ListenPort   uint16
	FirewallMark uint32
	Peers        []wireGuardNativePeer
}

type wireGuardNativePeer struct {
	PublicKey           wireGuardKey
	PresharedKey        wireGuardKey
	AllowedIPs          []string
	Endpoint            string
	PersistentKeepalive uint16
	LastHandshake       time.Time
	ReceivedBytes       uint64
	TransmittedBytes    uint64
}

// The optional backend keeps the portable engine and fake kernels independent
// of Linux UAPI structures. Every operation targets the verified interface
// index, with no userspace socket fallback or process execution.
type wireGuardKernel interface {
	ReadWireGuard(index int) (wireGuardDevice, error)
	ReplaceWireGuard(index int, wanted wireGuardDevice) error
	SetWireGuardUp(index int, up bool) error
}

// The native backend closes its private netlink sockets when the independent
// cleanup deadline expires. The ordinary kernel interface has no context on
// each syscall, so quarantine cannot borrow a canceled apply operation.
type wireGuardQuarantineKernel interface {
	QuarantineWireGuard(context.Context, string, int, func(InterfaceState) bool, func(wireGuardDevice) bool) (bool, error)
}

func secretFilePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) <= 4096 && !strings.ContainsAny(path, "\x00\r\n")
}

func validateWireGuard(d Desired) error {
	var errs []error
	add := func(format string, values ...any) { errs = append(errs, fmt.Errorf(format, values...)) }
	if len(d.WireGuard) > MaxWireGuardInterfaces {
		add("wireguard: at most %d devices are supported", MaxWireGuardInterfaces)
	}
	created := map[string]bool{}
	for _, l := range d.Links {
		created[l.Name] = l.Kind == "wireguard"
	}
	seen := map[string]bool{}
	for _, device := range d.WireGuard {
		if !validName(device.Interface) || !created[device.Interface] {
			add("wireguard %s: an explicitly created Link of kind wireguard is required", device.Interface)
		}
		if seen[device.Interface] {
			add("duplicate wireguard configuration for %s", device.Interface)
		}
		seen[device.Interface] = true
		if !secretFilePath(device.PrivateKeyFile) {
			add("wireguard %s: private_key_file must be a clean absolute path", device.Interface)
		}
		if device.ListenPort < 0 || device.ListenPort > 65535 {
			add("wireguard %s: listen_port must be 0..65535", device.Interface)
		}
		if len(device.Peers) > MaxWireGuardPeers {
			add("wireguard %s: at most %d peers are supported", device.Interface, MaxWireGuardPeers)
		}
		peers, prefixes := map[string]bool{}, map[string]bool{}
		count := 0
		for i, peer := range device.Peers {
			key, err := parseWireGuardKey(peer.PublicKey)
			if err != nil || key.zero() {
				add("wireguard %s peer %d: invalid or zero public key", device.Interface, i)
			}
			if peers[peer.PublicKey] {
				add("wireguard %s peer %d: duplicate public key", device.Interface, i)
			}
			peers[peer.PublicKey] = true
			if peer.PresharedKeyFile != "" && !secretFilePath(peer.PresharedKeyFile) {
				add("wireguard %s peer %d: preshared_key_file must be a clean absolute path", device.Interface, i)
			}
			if peer.PersistentKeepalive < 0 || peer.PersistentKeepalive > 65535 {
				add("wireguard %s peer %d: persistent_keepalive must be 0..65535 seconds", device.Interface, i)
			}
			if peer.Endpoint != "" {
				endpoint, err := netip.ParseAddrPort(peer.Endpoint)
				a := endpoint.Addr()
				if err != nil || endpoint.Port() == 0 || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() || a.Zone() != "" && (!a.Is6() || !validName(a.Zone())) || a.IsLinkLocalUnicast() && a.Is6() && a.Zone() == "" {
					add("wireguard %s peer %d: endpoint must be a numeric unicast IP:port, with a zone for IPv6 link-local", device.Interface, i)
				}
			}
			for j, raw := range peer.AllowedIPs {
				prefix, err := netip.ParsePrefix(raw)
				if err != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
					add("wireguard %s peer %d allowed IP %d: expected a canonical network prefix", device.Interface, i, j)
					continue
				}
				if prefixes[prefix.String()] {
					add("wireguard %s: the same allowed IP prefix cannot be assigned twice", device.Interface)
				}
				prefixes[prefix.String()] = true
			}
			count += len(peer.AllowedIPs)
		}
		if count > MaxWireGuardAllowedIPs {
			add("wireguard %s: at most %d allowed IP prefixes are supported", device.Interface, MaxWireGuardAllowedIPs)
		}
	}
	for _, link := range d.Interfaces {
		if created[link.Name] && !seen[link.Name] {
			add("wireguard %s: an UP interface requires a matching WireGuard configuration", link.Name)
		}
	}
	return errors.Join(errs...)
}

func normalizeWireGuard(device WireGuard) WireGuard {
	for i := range device.Peers {
		peer := &device.Peers[i]
		if peer.Endpoint != "" {
			endpoint, _ := netip.ParseAddrPort(peer.Endpoint)
			peer.Endpoint = endpoint.String()
		}
		for j, prefix := range peer.AllowedIPs {
			p, _ := netip.ParsePrefix(prefix)
			peer.AllowedIPs[j] = p.String()
		}
		sort.Strings(peer.AllowedIPs)
	}
	sort.Slice(device.Peers, func(i, j int) bool { return device.Peers[i].PublicKey < device.Peers[j].PublicKey })
	return device
}

func publicWireGuardState(name string, device wireGuardDevice, owned, quarantined bool) WireGuardState {
	state := WireGuardState{Interface: name, Index: device.Index, Owned: owned, Quarantined: quarantined, ListenPort: int(device.ListenPort), FirewallMark: device.FirewallMark}
	if !device.PublicKey.zero() {
		state.PublicKey = device.PublicKey.encoded()
	}
	for _, peer := range device.Peers {
		state.Peers = append(state.Peers, WireGuardPeerState{PublicKey: peer.PublicKey.encoded(), AllowedIPs: append([]string(nil), peer.AllowedIPs...), Endpoint: peer.Endpoint, PersistentKeepalive: int(peer.PersistentKeepalive), LastHandshake: peer.LastHandshake, ReceivedBytes: peer.ReceivedBytes, TransmittedBytes: peer.TransmittedBytes})
	}
	sort.Slice(state.Peers, func(i, j int) bool { return state.Peers[i].PublicKey < state.Peers[j].PublicKey })
	return state
}
