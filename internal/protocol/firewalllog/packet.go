// SPDX-License-Identifier: BSD-3-Clause

package firewalllog

import "strings"

// Keep the original routerd firewall logger's TCP flag decoding and ordering.
func tcpFlagsFromIPPacket(payload []byte) string {
	if len(payload) < 1 {
		return ""
	}
	switch payload[0] >> 4 {
	case 4:
		if len(payload) < 20 {
			return ""
		}
		ihl := int(payload[0]&0x0f) * 4
		if ihl < 20 || len(payload) < ihl || payload[9] != 6 {
			return ""
		}
		return tcpFlagsFromTransport(payload[ihl:])
	case 6:
		next, offset, ok := ipv6TransportOffset(payload)
		if !ok || next != 6 || len(payload) < offset {
			return ""
		}
		return tcpFlagsFromTransport(payload[offset:])
	default:
		return ""
	}
}

func tcpFlagsFromTransport(payload []byte) string {
	if len(payload) < 14 {
		return ""
	}
	return tcpFlagsFromByte(payload[13])
}

func tcpFlagsFromByte(flags byte) string {
	var out []string
	for _, flag := range []struct {
		mask byte
		name string
	}{
		{0x02, "SYN"},
		{0x10, "ACK"},
		{0x08, "PSH"},
		{0x04, "RST"},
		{0x01, "FIN"},
		{0x20, "URG"},
	} {
		if flags&flag.mask != 0 {
			out = append(out, flag.name)
		}
	}
	return strings.Join(out, ",")
}

func ipv6TransportOffset(payload []byte) (byte, int, bool) {
	if len(payload) < 40 {
		return 0, 0, false
	}
	next := payload[6]
	offset := 40
	for {
		switch next {
		case 0, 43, 60:
			if len(payload) < offset+2 {
				return 0, 0, false
			}
			headerLen := (int(payload[offset+1]) + 1) * 8
			next = payload[offset]
			offset += headerLen
		case 44:
			if len(payload) < offset+8 {
				return 0, 0, false
			}
			next = payload[offset]
			offset += 8
		case 51:
			if len(payload) < offset+2 {
				return 0, 0, false
			}
			headerLen := (int(payload[offset+1]) + 2) * 4
			next = payload[offset]
			offset += headerLen
		default:
			return next, offset, offset <= len(payload)
		}
		if offset > len(payload) {
			return 0, 0, false
		}
	}
}
