//go:build linux

package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const wireGuardGenlVersion = 1

func wireGuardFamily() (*netlink.GenlFamily, error) {
	family, err := netlink.GenlFamilyGet("wireguard")
	if err != nil {
		return nil, fmt.Errorf("Linux WireGuard generic-netlink family: %w", err)
	}
	if family.Version != wireGuardGenlVersion {
		return nil, fmt.Errorf("unsupported WireGuard generic-netlink version %d", family.Version)
	}
	return family, nil
}

func wireGuardAttr(kind int, data []byte) *nl.RtAttr { return nl.NewRtAttr(kind, data) }
func wireGuardUint16(value uint16) []byte            { return binary.NativeEndian.AppendUint16(nil, value) }
func wireGuardUint32(value uint32) []byte            { return binary.NativeEndian.AppendUint32(nil, value) }

func wireGuardRequest(family uint16, command uint8, index int, flags int) *nl.NetlinkRequest {
	req := nl.NewNetlinkRequest(int(family), flags)
	req.AddData(&nl.Genlmsg{Command: command, Version: wireGuardGenlVersion})
	req.AddData(wireGuardAttr(unix.WGDEVICE_A_IFINDEX, wireGuardUint32(uint32(index))))
	return req
}

func wireGuardAttributes(data []byte) ([]syscall.NetlinkRouteAttr, error) {
	attrs, err := nl.ParseRouteAttr(data)
	if err != nil {
		return nil, errors.New("malformed WireGuard netlink attributes")
	}
	for i := range attrs {
		attrs[i].Attr.Type &^= unix.NLA_F_NESTED | unix.NLA_F_NET_BYTEORDER
	}
	return attrs, nil
}

func parseWireGuardEndpoint(data []byte) (string, error) {
	if len(data) < 2 {
		return "", errors.New("malformed WireGuard endpoint")
	}
	family := binary.NativeEndian.Uint16(data[:2])
	var addr netip.Addr
	switch family {
	case unix.AF_INET:
		if len(data) != 16 {
			return "", errors.New("malformed WireGuard IPv4 endpoint")
		}
		var value [4]byte
		copy(value[:], data[4:8])
		addr = netip.AddrFrom4(value)
	case unix.AF_INET6:
		if len(data) != 28 {
			return "", errors.New("malformed WireGuard IPv6 endpoint")
		}
		var value [16]byte
		copy(value[:], data[8:24])
		addr = netip.AddrFrom16(value)
		if index := binary.NativeEndian.Uint32(data[24:28]); index != 0 {
			addr = addr.WithZone(strconv.FormatUint(uint64(index), 10))
		}
	default:
		return "", errors.New("unsupported WireGuard endpoint family")
	}
	return netip.AddrPortFrom(addr, binary.BigEndian.Uint16(data[2:4])).String(), nil
}

func encodeWireGuardEndpoint(endpoint string) ([]byte, error) {
	value, err := netip.ParseAddrPort(endpoint)
	if err != nil {
		return nil, errors.New("invalid WireGuard endpoint")
	}
	addr := value.Addr()
	if addr.Is4() {
		data := make([]byte, 16)
		binary.NativeEndian.PutUint16(data[:2], unix.AF_INET)
		binary.BigEndian.PutUint16(data[2:4], value.Port())
		copy(data[4:8], addr.AsSlice())
		return data, nil
	}
	data := make([]byte, 28)
	binary.NativeEndian.PutUint16(data[:2], unix.AF_INET6)
	binary.BigEndian.PutUint16(data[2:4], value.Port())
	copy(data[8:24], addr.AsSlice())
	if zone := addr.Zone(); zone != "" {
		index, numberErr := strconv.ParseUint(zone, 10, 32)
		if numberErr != nil {
			link, err := netlink.LinkByName(zone)
			if err != nil {
				return nil, errors.New("WireGuard endpoint zone interface does not exist")
			}
			index = uint64(link.Attrs().Index)
		}
		if index == 0 {
			return nil, errors.New("WireGuard endpoint zone must identify an interface")
		}
		binary.NativeEndian.PutUint32(data[24:28], uint32(index))
	}
	return data, nil
}

func parseWireGuardAllowedIP(data []byte) (string, error) {
	attrs, err := wireGuardAttributes(data)
	if err != nil {
		return "", err
	}
	var family uint16
	var ip []byte
	mask := -1
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case unix.WGALLOWEDIP_A_FAMILY:
			if len(attr.Value) != 2 {
				return "", errors.New("malformed WireGuard allowed IP family")
			}
			family = binary.NativeEndian.Uint16(attr.Value)
		case unix.WGALLOWEDIP_A_IPADDR:
			ip = attr.Value
		case unix.WGALLOWEDIP_A_CIDR_MASK:
			if len(attr.Value) != 1 {
				return "", errors.New("malformed WireGuard allowed IP mask")
			}
			mask = int(attr.Value[0])
		default:
			return "", errors.New("unsupported WireGuard allowed IP attribute")
		}
	}
	addr, valid := netip.AddrFromSlice(ip)
	if !valid || family != unix.AF_INET && family != unix.AF_INET6 || family == unix.AF_INET && len(ip) != 4 || family == unix.AF_INET6 && len(ip) != 16 || mask < 0 || mask > addr.BitLen() || addr.Is4In6() {
		return "", errors.New("malformed WireGuard allowed IP")
	}
	return netip.PrefixFrom(addr, mask).Masked().String(), nil
}

func parseWireGuardPeer(data []byte) (wireGuardNativePeer, error) {
	var peer wireGuardNativePeer
	attrs, err := wireGuardAttributes(data)
	if err != nil {
		return peer, err
	}
	for _, attr := range attrs {
		value := attr.Value
		switch attr.Attr.Type {
		case unix.WGPEER_A_PUBLIC_KEY, unix.WGPEER_A_PRESHARED_KEY:
			if len(value) != 32 {
				return peer, errors.New("malformed WireGuard peer key length")
			}
			if attr.Attr.Type == unix.WGPEER_A_PUBLIC_KEY {
				copy(peer.PublicKey[:], value)
			} else {
				copy(peer.PresharedKey[:], value)
			}
		case unix.WGPEER_A_PERSISTENT_KEEPALIVE_INTERVAL:
			if len(value) != 2 {
				return peer, errors.New("malformed WireGuard keepalive")
			}
			peer.PersistentKeepalive = binary.NativeEndian.Uint16(value)
		case unix.WGPEER_A_ENDPOINT:
			peer.Endpoint, err = parseWireGuardEndpoint(value)
			if err != nil {
				return peer, err
			}
		case unix.WGPEER_A_RX_BYTES, unix.WGPEER_A_TX_BYTES:
			if len(value) != 8 {
				return peer, errors.New("malformed WireGuard counters")
			}
			if attr.Attr.Type == unix.WGPEER_A_RX_BYTES {
				peer.ReceivedBytes = binary.NativeEndian.Uint64(value)
			} else {
				peer.TransmittedBytes = binary.NativeEndian.Uint64(value)
			}
		case unix.WGPEER_A_LAST_HANDSHAKE_TIME:
			if len(value) != 16 {
				return peer, errors.New("malformed WireGuard handshake timestamp")
			}
			seconds, nanos := int64(binary.NativeEndian.Uint64(value[:8])), int64(binary.NativeEndian.Uint64(value[8:]))
			if seconds < 0 || nanos < 0 || nanos >= int64(time.Second) {
				return peer, errors.New("invalid WireGuard handshake timestamp")
			}
			if seconds != 0 || nanos != 0 {
				peer.LastHandshake = time.Unix(seconds, nanos).UTC()
			}
		case unix.WGPEER_A_PROTOCOL_VERSION:
			if len(value) != 4 || binary.NativeEndian.Uint32(value) != 1 {
				return peer, errors.New("unsupported WireGuard peer protocol version")
			}
		case unix.WGPEER_A_ALLOWEDIPS:
			entries, err := wireGuardAttributes(value)
			if err != nil {
				return peer, err
			}
			for _, entry := range entries {
				prefix, err := parseWireGuardAllowedIP(entry.Value)
				if err != nil {
					return peer, err
				}
				peer.AllowedIPs = append(peer.AllowedIPs, prefix)
			}
		case unix.WGPEER_A_UNSPEC:
			// The kernel uses an empty UNSPEC as 64-bit alignment padding.
			if len(value) != 0 {
				return peer, errors.New("unexpected WireGuard peer padding")
			}
		default:
			return peer, errors.New("unsupported WireGuard peer attribute")
		}
	}
	if peer.PublicKey.zero() {
		return peer, errors.New("WireGuard peer readback is missing its public key")
	}
	return peer, nil
}

func (k *linuxKernel) ReadWireGuard(index int) (wireGuardDevice, error) {
	family, err := wireGuardFamily()
	if err != nil {
		return wireGuardDevice{Index: index}, err
	}
	req := wireGuardRequest(family.ID, unix.WG_CMD_GET_DEVICE, index, unix.NLM_F_DUMP)
	return readWireGuardRequest(index, req, family.ID)
}

func readWireGuardRequest(index int, req *nl.NetlinkRequest, familyID uint16) (wireGuardDevice, error) {
	device := wireGuardDevice{Index: index}
	peerIndex := map[wireGuardKey]int{}
	allowed := 0
	observed := false
	var parseErr error
	err := req.ExecuteIter(unix.NETLINK_GENERIC, familyID, func(message []byte) bool {
		defer clear(message)
		if len(message) < nl.SizeofGenlmsg || message[0] != unix.WG_CMD_GET_DEVICE || message[1] != wireGuardGenlVersion {
			parseErr = errors.New("malformed WireGuard device message")
			return false
		}
		attrs, err := wireGuardAttributes(message[nl.SizeofGenlmsg:])
		if err != nil {
			parseErr = err
			return false
		}
		for _, attr := range attrs {
			value := attr.Value
			switch attr.Attr.Type {
			case unix.WGDEVICE_A_IFINDEX:
				if len(value) != 4 || int(binary.NativeEndian.Uint32(value)) != index {
					parseErr = errors.New("WireGuard readback interface identity differs")
					return false
				}
				observed = true
			case unix.WGDEVICE_A_IFNAME:
				// The request and ownership checks use IFINDEX, never IFNAME.
			case unix.WGDEVICE_A_PRIVATE_KEY, unix.WGDEVICE_A_PUBLIC_KEY:
				if len(value) != 32 {
					parseErr = errors.New("malformed WireGuard device key length")
					return false
				}
				if attr.Attr.Type == unix.WGDEVICE_A_PRIVATE_KEY {
					copy(device.PrivateKey[:], value)
				} else {
					copy(device.PublicKey[:], value)
				}
			case unix.WGDEVICE_A_LISTEN_PORT:
				if len(value) != 2 {
					parseErr = errors.New("malformed WireGuard listen port")
					return false
				}
				device.ListenPort = binary.NativeEndian.Uint16(value)
			case unix.WGDEVICE_A_FWMARK:
				if len(value) != 4 {
					parseErr = errors.New("malformed WireGuard firewall mark")
					return false
				}
				device.FirewallMark = binary.NativeEndian.Uint32(value)
			case unix.WGDEVICE_A_PEERS:
				entries, err := wireGuardAttributes(value)
				if err != nil {
					parseErr = err
					return false
				}
				for _, entry := range entries {
					peer, err := parseWireGuardPeer(entry.Value)
					if err != nil {
						parseErr = err
						return false
					}
					allowed += len(peer.AllowedIPs)
					if allowed > MaxWireGuardAllowedIPs {
						parseErr = errors.New("WireGuard readback exceeds allowed IP limit")
						return false
					}
					if previous, exists := peerIndex[peer.PublicKey]; exists {
						// Multipart GET fragments repeat a peer's public key, then
						// continue its AllowedIPs without repeating other fields.
						if previous != len(device.Peers)-1 || !peer.PresharedKey.zero() || peer.Endpoint != "" || peer.PersistentKeepalive != 0 || !peer.LastHandshake.IsZero() || peer.ReceivedBytes != 0 || peer.TransmittedBytes != 0 {
							parseErr = errors.New("unexpected repeated WireGuard peer")
							return false
						}
						device.Peers[previous].AllowedIPs = append(device.Peers[previous].AllowedIPs, peer.AllowedIPs...)
					} else {
						if len(device.Peers) >= MaxWireGuardPeers {
							parseErr = errors.New("WireGuard readback exceeds peer limit")
							return false
						}
						peerIndex[peer.PublicKey] = len(device.Peers)
						device.Peers = append(device.Peers, peer)
					}
				}
			default:
				parseErr = errors.New("unsupported WireGuard device attribute")
				return false
			}
		}
		return true
	})
	if err != nil || parseErr != nil {
		return device, errors.Join(err, parseErr)
	}
	if !observed {
		return device, errors.New("WireGuard interface not found in kernel readback")
	}
	for i := range device.Peers {
		sort.Strings(device.Peers[i].AllowedIPs)
	}
	sort.Slice(device.Peers, func(i, j int) bool { return device.Peers[i].PublicKey.encoded() < device.Peers[j].PublicKey.encoded() })
	return device, nil
}

func wireGuardPeerSegment(peer wireGuardNativePeer, prefixes []string, first bool) (*nl.RtAttr, error) {
	entry := wireGuardAttr(1|unix.NLA_F_NESTED, nil)
	entry.AddChild(wireGuardAttr(unix.WGPEER_A_PUBLIC_KEY, peer.PublicKey[:]))
	flags := uint32(unix.WGPEER_F_UPDATE_ONLY)
	if first {
		flags = unix.WGPEER_F_REPLACE_ALLOWEDIPS
		entry.AddChild(wireGuardAttr(unix.WGPEER_A_PRESHARED_KEY, peer.PresharedKey[:]))
		entry.AddChild(wireGuardAttr(unix.WGPEER_A_PERSISTENT_KEEPALIVE_INTERVAL, wireGuardUint16(peer.PersistentKeepalive)))
		if peer.Endpoint != "" {
			endpoint, err := encodeWireGuardEndpoint(peer.Endpoint)
			if err != nil {
				return nil, err
			}
			entry.AddChild(wireGuardAttr(unix.WGPEER_A_ENDPOINT, endpoint))
		}
	}
	entry.AddChild(wireGuardAttr(unix.WGPEER_A_FLAGS, wireGuardUint32(flags)))
	allowed := wireGuardAttr(unix.WGPEER_A_ALLOWEDIPS|unix.NLA_F_NESTED, nil)
	for _, raw := range prefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, errors.New("invalid WireGuard allowed IP")
		}
		family := uint16(unix.AF_INET)
		if prefix.Addr().Is6() {
			family = unix.AF_INET6
		}
		nested := wireGuardAttr(1|unix.NLA_F_NESTED, nil)
		nested.AddChild(wireGuardAttr(unix.WGALLOWEDIP_A_FAMILY, wireGuardUint16(family)))
		nested.AddChild(wireGuardAttr(unix.WGALLOWEDIP_A_IPADDR, prefix.Addr().AsSlice()))
		nested.AddChild(wireGuardAttr(unix.WGALLOWEDIP_A_CIDR_MASK, []byte{byte(prefix.Bits())}))
		allowed.AddChild(nested)
	}
	entry.AddChild(allowed)
	return entry, nil
}

// Whole-set replacement is deliberately NOT called atomic: WireGuard SET can
// partially mutate a device even within one message. The engine journals its
// intent and holds the owned device DOWN until every batch and readback succeeds.
func (k *linuxKernel) ReplaceWireGuard(index int, wanted wireGuardDevice) error {
	family, err := wireGuardFamily()
	if err != nil {
		return err
	}
	var segments []*nl.RtAttr
	for _, peer := range wanted.Peers {
		if len(peer.AllowedIPs) == 0 {
			entry, err := wireGuardPeerSegment(peer, nil, true)
			if err != nil {
				return err
			}
			segments = append(segments, entry)
		}
		for offset := 0; offset < len(peer.AllowedIPs); offset += 128 {
			end := min(offset+128, len(peer.AllowedIPs))
			entry, err := wireGuardPeerSegment(peer, peer.AllowedIPs[offset:end], offset == 0)
			if err != nil {
				return err
			}
			segments = append(segments, entry)
		}
	}
	first := true
	for first || len(segments) > 0 {
		req := wireGuardRequest(family.ID, unix.WG_CMD_SET_DEVICE, index, unix.NLM_F_ACK)
		if first {
			req.AddData(wireGuardAttr(unix.WGDEVICE_A_FLAGS, wireGuardUint32(unix.WGDEVICE_F_REPLACE_PEERS)))
			req.AddData(wireGuardAttr(unix.WGDEVICE_A_PRIVATE_KEY, wanted.PrivateKey[:]))
			req.AddData(wireGuardAttr(unix.WGDEVICE_A_LISTEN_PORT, wireGuardUint16(wanted.ListenPort)))
			req.AddData(wireGuardAttr(unix.WGDEVICE_A_FWMARK, wireGuardUint32(wanted.FirewallMark)))
		}
		peers := wireGuardAttr(unix.WGDEVICE_A_PEERS|unix.NLA_F_NESTED, nil)
		for len(segments) > 0 && peers.Len()+segments[0].Len() <= 24*1024 {
			peers.AddChild(segments[0])
			segments = segments[1:]
		}
		req.AddData(peers)
		if _, err := req.Execute(unix.NETLINK_GENERIC, 0); err != nil {
			return &partialMutationError{fmt.Errorf("WireGuard replacement batch: %w", err)}
		}
		first = false
	}
	return nil
}

func (k *linuxKernel) SetWireGuardUp(index int, up bool) error {
	link, err := netlink.LinkByIndex(index)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" {
		return errors.New("WireGuard interface index no longer identifies a WireGuard device")
	}
	if up {
		return netlink.LinkSetUp(link)
	}
	return netlink.LinkSetDown(link)
}
