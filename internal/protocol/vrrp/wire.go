package vrrp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

const protocol = 112

var (
	group4                = netip.MustParseAddr("224.0.0.18")
	group6                = netip.MustParseAddr("ff02::12")
	all6                  = netip.MustParseAddr("ff02::1")
	errOtherVirtualRouter = errors.New("advertisement belongs to another virtual router")
)

type advertisement struct {
	source    netip.Addr
	priority  uint8
	interval  time.Duration
	addresses []netip.Addr
}

// An IP datagram with actual kernel receive metadata, not a fabricated Ethernet
// frame. Unicast checks use the real destination and an explicit peer allowlist.
type datagram struct {
	source, destination netip.Addr
	hopLimit            int
	payload             []byte
}

func multicastGroup(family string) netip.Addr {
	if family == "ipv6" {
		return group6
	}
	return group4
}

func parseDatagram(c Config, local netip.Addr, p datagram) (advertisement, error) {
	if p.hopLimit != 255 || !addressInFamily(p.source, c.Family) {
		return advertisement{}, errors.New("invalid VRRP source or TTL/hop limit")
	}
	if len(c.UnicastPeers) == 0 {
		if p.destination != multicastGroup(c.Family) || c.Family == "ipv6" && !p.source.IsLinkLocalUnicast() {
			return advertisement{}, errors.New("invalid VRRP multicast source or destination")
		}
	} else {
		peer := false
		for _, address := range c.UnicastPeers {
			peer = peer || address == p.source
		}
		if !peer || p.destination != local {
			return advertisement{}, errors.New("VRRP unicast source is not a configured peer or destination is not local")
		}
	}
	return parseAdvertisement(c, p.source, p.destination, p.payload)
}

func checksum(parts ...[]byte) uint16 {
	var sum uint32
	var high byte
	odd := false
	for _, b := range parts {
		for _, next := range b {
			if !odd {
				high, odd = next, true
			} else {
				sum += uint32(high)<<8 | uint32(next)
				odd = false
			}
		}
	}
	if odd {
		sum += uint32(high) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func pseudo6(source, dest netip.Addr, length int, nextHeader byte) []byte {
	p := make([]byte, 40)
	s, d := source.As16(), dest.As16()
	copy(p, s[:])
	copy(p[16:], d[:])
	binary.BigEndian.PutUint32(p[32:], uint32(length))
	p[39] = nextHeader
	return p
}

func marshalAdvertisement(c Config, source, destination netip.Addr, priority uint8) []byte {
	width := 4
	if c.Family == "ipv6" {
		width = 16
	}
	b := make([]byte, 8+width*len(c.Addresses))
	b[0], b[1], b[2], b[3] = 0x31, byte(c.VirtualRouterID), priority, byte(len(c.Addresses))
	binary.BigEndian.PutUint16(b[4:], uint16(c.AdvertIntervalCentiseconds))
	for i, a := range c.addresses() {
		copy(b[8+i*width:], a.AsSlice())
	}
	value := checksum(b) // RFC 9568: no pseudo-header for IPv4.
	if c.Family == "ipv6" {
		value = checksum(pseudo6(source, destination, len(b), protocol), b)
	}
	binary.BigEndian.PutUint16(b[6:], value)
	return b
}

func parseAdvertisement(c Config, source, destination netip.Addr, payload []byte) (advertisement, error) {
	if len(payload) >= 2 && int(payload[1]) != c.VirtualRouterID {
		return advertisement{}, errOtherVirtualRouter
	}
	if len(payload) < 8 || payload[0] != 0x31 || payload[3] == 0 {
		return advertisement{}, errors.New("invalid VRRP version, type, VRID, or address count")
	}
	width := 4
	if c.Family == "ipv6" {
		width = 16
	}
	if len(payload) != 8+width*int(payload[3]) {
		return advertisement{}, errors.New("incomplete VRRP address list or trailing bytes")
	}
	validChecksum := checksum(payload) == 0
	if c.Family == "ipv6" {
		validChecksum = checksum(pseudo6(source, destination, len(payload), protocol), payload) == 0
	}
	if !validChecksum {
		return advertisement{}, errors.New("invalid VRRP checksum")
	}
	// The high four reserved bits must be ignored by receivers.
	centiseconds := binary.BigEndian.Uint16(payload[4:6]) & 0xfff
	if centiseconds == 0 {
		return advertisement{}, errors.New("zero VRRP advertisement interval")
	}
	addresses := make([]netip.Addr, int(payload[3]))
	for i := range addresses {
		a, _ := netip.AddrFromSlice(payload[8+i*width : 8+(i+1)*width])
		if !addressInFamily(a, c.Family) || c.Family == "ipv6" && i == 0 && !a.IsLinkLocalUnicast() {
			return advertisement{}, errors.New("invalid virtual address in VRRP advertisement")
		}
		addresses[i] = a
	}
	// Strict comparison detects VRID collisions instead of withdrawing a VIP
	// based on an unrelated virtual router. Order is deliberately irrelevant.
	configured := c.addresses()
	if len(addresses) != len(configured) {
		return advertisement{}, errors.New("VRRP virtual address list differs from configuration")
	}
	set := make(map[netip.Addr]bool, len(addresses))
	for _, a := range addresses {
		set[a] = true
	}
	for _, a := range configured {
		if !set[a] {
			return advertisement{}, errors.New("VRRP virtual address list differs from configuration")
		}
	}
	return advertisement{source: source, priority: payload[2], interval: time.Duration(centiseconds) * 10 * time.Millisecond, addresses: addresses}, nil
}

func groupMAC(family string) net.HardwareAddr {
	if family == "ipv6" {
		return net.HardwareAddr{0x33, 0x33, 0, 0, 0, 0x12}
	}
	return net.HardwareAddr{1, 0, 0x5e, 0, 0, 0x12}
}

func ethernet(payload []byte, source, dest net.HardwareAddr, etherType uint16) []byte {
	b := make([]byte, 14+len(payload))
	copy(b, dest)
	copy(b[6:], source)
	binary.BigEndian.PutUint16(b[12:], etherType)
	copy(b[14:], payload)
	return b
}

func ipv4Packet(source, dest netip.Addr, payload []byte) []byte {
	b := make([]byte, 20+len(payload))
	b[0], b[1], b[8], b[9] = 0x45, 0xc0, 255, protocol
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	copy(b[12:], source.AsSlice())
	copy(b[16:], dest.AsSlice())
	binary.BigEndian.PutUint16(b[10:], checksum(b[:20]))
	copy(b[20:], payload)
	return b
}

func ipv6Packet(source, dest netip.Addr, payload []byte, nextHeader byte) []byte {
	b := make([]byte, 40+len(payload))
	b[0], b[6], b[7] = 0x6c, nextHeader, 255
	binary.BigEndian.PutUint16(b[4:], uint16(len(payload)))
	copy(b[8:], source.AsSlice())
	copy(b[24:], dest.AsSlice())
	copy(b[40:], payload)
	return b
}

func datagramFrame(family string, vrid int, p datagram) []byte {
	typeID := uint16(0x0800)
	ip := ipv4Packet
	if family == "ipv6" {
		typeID = 0x86dd
		ip = func(s, d netip.Addr, b []byte) []byte { return ipv6Packet(s, d, b, protocol) }
	}
	return ethernet(ip(p.source, p.destination, p.payload), VirtualMAC(family, vrid), groupMAC(family), typeID)
}

func notificationFrames(c Config) [][]byte {
	mac := VirtualMAC(c.Family, c.VirtualRouterID)
	frames := make([][]byte, 0, len(c.Addresses))
	for _, address := range c.addresses() {
		if c.Family == "ipv4" {
			// Gratuitous ARP request: sender and target protocol addresses are
			// equal, and both hardware fields contain the virtual MAC.
			arp := make([]byte, 28)
			binary.BigEndian.PutUint16(arp[0:], 1)
			binary.BigEndian.PutUint16(arp[2:], 0x0800)
			arp[4], arp[5] = 6, 4
			binary.BigEndian.PutUint16(arp[6:], 1)
			copy(arp[8:], mac)
			copy(arp[14:], address.AsSlice())
			copy(arp[18:], mac)
			copy(arp[24:], address.AsSlice())
			frames = append(frames, ethernet(arp, mac, net.HardwareAddr{255, 255, 255, 255, 255, 255}, 0x0806))
			continue
		}
		na := make([]byte, 32)
		na[0], na[4] = 136, 0xa0 // Router=1, Solicited=0, Override=1.
		copy(na[8:], address.AsSlice())
		na[24], na[25] = 2, 1 // Target Link-Layer Address option.
		copy(na[26:], mac)
		binary.BigEndian.PutUint16(na[2:], checksum(pseudo6(address, all6, len(na), 58), na))
		frames = append(frames, ethernet(ipv6Packet(address, all6, na, 58), mac, net.HardwareAddr{0x33, 0x33, 0, 0, 0, 1}, 0x86dd))
	}
	return frames
}

func frameError(err error) string { return fmt.Sprintf("discard VRRP advertisement: %v", err) }
