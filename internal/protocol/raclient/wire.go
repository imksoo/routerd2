// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"encoding/binary"
	"net/netip"
	"strings"
)

type packet struct {
	payload             []byte
	source, destination netip.Addr
	hopLimit, ifindex   int
}
type prefixOption struct {
	prefix             netip.Prefix
	onLink, autonomous bool
	valid, preferred   uint32
}
type routeOption struct {
	prefix     netip.Prefix
	preference int
	lifetime   uint32
}
type dnsOption struct {
	addresses []netip.Addr
	domains   []string
	lifetime  uint32
}
type advertisement struct {
	router                netip.Addr
	managed, other        bool
	preference            int
	lifetime              uint32
	mtu, hopLimit         int
	reachable, retransmit uint32
	prefixes              []prefixOption
	routes                []routeOption
	dns                   []dnsOption
}

func preference(flags byte) (int, bool) {
	switch (flags >> 3) & 3 {
	case 0:
		return 0, true
	case 1:
		return 1, true
	case 3:
		return -1, true
	default:
		return 0, false
	}
}

// Invalid framing or header checks reject the datagram. A framed but unusable
// option is ignored without losing the router or independent usable options.
func parse(p packet, index, linkMTU int) (advertisement, error) {
	b := p.payload
	if len(b) < 16 || len(b) > 65535 || b[0] != 134 || b[1] != 0 || p.hopLimit != 255 || p.ifindex != index || !p.source.Is6() || !p.source.IsLinkLocalUnicast() || p.source.Zone() != "" || !p.destination.Is6() || p.destination.Is4In6() || p.destination.IsUnspecified() || p.destination.Zone() != "" {
		return advertisement{}, snapshotError("header/source/interface/hop limit")
	}
	if checksum(b, p.source, p.destination) != 0 {
		return advertisement{}, snapshotError("checksum")
	}
	pref, _ := preference(b[5]) // Reserved header preference is medium, RFC 4191.
	a := advertisement{router: p.source, managed: b[5]&0x80 != 0, other: b[5]&0x40 != 0, preference: pref, lifetime: uint32(binary.BigEndian.Uint16(b[6:])), hopLimit: int(b[4]), reachable: binary.BigEndian.Uint32(b[8:]), retransmit: binary.BigEndian.Uint32(b[12:])}
optionLoop:
	for at := 16; at < len(b); {
		if at+2 > len(b) || b[at+1] == 0 {
			return advertisement{}, snapshotError("zero/truncated option length")
		}
		size := int(b[at+1]) * 8
		if at+size > len(b) {
			return advertisement{}, snapshotError("truncated option")
		}
		o := b[at : at+size]
		at += size
		switch o[0] {
		case 1:
			// Source LLA is informational. The kernel remains the ND/NUD owner.
		case 3:
			if size != 32 {
				continue
			}
			if o[2] > 128 {
				continue
			}
			var raw [16]byte
			copy(raw[:], o[16:32])
			prefix := netip.PrefixFrom(netip.AddrFrom16(raw), int(o[2])).Masked()
			valid, preferred := binary.BigEndian.Uint32(o[4:]), binary.BigEndian.Uint32(o[8:])
			if !validPrefix(prefix) || preferred > valid {
				continue
			}
			a.prefixes = append(a.prefixes, prefixOption{prefix, o[3]&0x80 != 0, o[3]&0x40 != 0, valid, preferred})
		case 5:
			if size != 8 {
				continue
			}
			mtu := uint64(binary.BigEndian.Uint32(o[4:]))
			if mtu >= 1280 && mtu <= uint64(linkMTU) && mtu <= 65535 {
				a.mtu = int(mtu)
			}
		case 24:
			if size != 8 && size != 16 && size != 24 {
				continue
			}
			bits := int(o[2])
			pref, validPref := preference(o[3])
			if bits > 128 || bits > 64 && size != 24 || bits > 0 && size == 8 || !validPref {
				continue
			}
			var raw [16]byte
			copy(raw[:], o[8:])
			prefix := netip.PrefixFrom(netip.AddrFrom16(raw), bits).Masked()
			if !validPrefix(prefix) {
				continue
			}
			a.routes = append(a.routes, routeOption{prefix, pref, binary.BigEndian.Uint32(o[4:])})
		case 25:
			if size < 24 || (size-8)%16 != 0 {
				continue
			}
			d := dnsOption{lifetime: binary.BigEndian.Uint32(o[4:])}
			for pos := 8; pos < size; pos += 16 {
				var raw [16]byte
				copy(raw[:], o[pos:pos+16])
				address := netip.AddrFrom16(raw)
				if address.Is4In6() || address.IsUnspecified() || address.IsMulticast() {
					continue optionLoop
				}
				d.addresses = append(d.addresses, address)
			}
			a.dns = append(a.dns, d)
		case 31:
			if size < 16 {
				continue
			}
			domains, err := decodeDomains(o[8:])
			if err != nil {
				continue
			}
			a.dns = append(a.dns, dnsOption{domains: domains, lifetime: binary.BigEndian.Uint32(o[4:])})
		}
	}
	return a, nil
}

func decodeDomains(b []byte) ([]string, error) {
	var out []string
	for at := 0; at < len(b); {
		if b[at] == 0 {
			for _, x := range b[at:] {
				if x != 0 {
					return nil, snapshotError("DNSSL padding")
				}
			}
			break
		}
		var labels []string
		for {
			if at >= len(b) {
				return nil, snapshotError("unterminated DNSSL")
			}
			n := int(b[at])
			at++
			if n == 0 {
				break
			}
			if n > 63 || at+n > len(b) {
				return nil, snapshotError("DNSSL label/compression")
			}
			labels = append(labels, strings.ToLower(string(b[at:at+n])))
			at += n
		}
		domain := strings.Join(labels, ".")
		if !validDomain(domain) {
			return nil, snapshotError("DNSSL domain")
		}
		out = append(out, domain)
	}
	if len(out) == 0 {
		return nil, snapshotError("empty DNSSL")
	}
	return out, nil
}

func checksum(payload []byte, source, destination netip.Addr) uint16 {
	var sum uint32
	add := func(b []byte) {
		for len(b) >= 2 {
			sum += uint32(binary.BigEndian.Uint16(b))
			b = b[2:]
		}
		if len(b) != 0 {
			sum += uint32(b[0]) << 8
		}
	}
	a, b := source.As16(), destination.As16()
	add(a[:])
	add(b[:])
	var pseudo [8]byte
	binary.BigEndian.PutUint32(pseudo[:4], uint32(len(payload)))
	pseudo[7] = 58
	add(pseudo[:])
	add(payload)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
