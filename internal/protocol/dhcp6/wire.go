//go:build linux

package dhcp6

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

const (
	solicit     byte = 1
	advertise   byte = 2
	request     byte = 3
	renew       byte = 5
	rebind      byte = 6
	reply       byte = 7
	decline     byte = 9
	information byte = 11
)

type option struct {
	code  uint16
	value []byte
}

func options(b []byte) ([]option, error) {
	var out []option
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("short DHCPv6 option")
		}
		code, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		if n > len(b) {
			return nil, errors.New("truncated DHCPv6 option")
		}
		out = append(out, option{code, b[:n]})
		b = b[n:]
	}
	return out, nil
}
func appendOption(b []byte, code uint16, v []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, code)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	return append(b, v...)
}

type assignment struct {
	prefix           netip.Prefix
	preferred, valid uint32
}
type association struct {
	iaid, t1, t2 uint32
	status       uint16
	leases       []assignment
}
type message struct {
	kind                       byte
	xid                        uint32
	client, server             []byte
	status                     uint16
	na, pd                     *association
	dns, domains, sntp         []string
	aftr                       string
	refresh                    uint32
	maxSolicit, maxInformation uint32
	preference                 byte
}

func decode(packet []byte, iaid uint32) (message, error) {
	if len(packet) < 4 {
		return message{}, errors.New("short DHCPv6 message")
	}
	m := message{kind: packet[0], xid: uint32(packet[1])<<16 | uint32(packet[2])<<8 | uint32(packet[3]), refresh: 86400}
	opts, err := options(packet[4:])
	if err != nil {
		return m, err
	}
	seen := map[uint16]bool{}
	for _, o := range opts {
		switch o.code {
		case 1, 2, 13:
			if seen[o.code] {
				return m, errors.New("duplicate DHCPv6 option")
			}
			seen[o.code] = true
		}
		switch o.code {
		case 1:
			m.client = append([]byte(nil), o.value...)
		case 2:
			m.server = append([]byte(nil), o.value...)
		case 3, 25:
			ia, e := decodeIA(o.code, o.value)
			if e != nil || ia.iaid != iaid {
				continue
			}
			if o.code == 3 && m.na == nil {
				m.na = &ia
			} else if o.code == 25 && m.pd == nil {
				m.pd = &ia
			}
		case 7:
			if len(o.value) != 1 {
				continue
			}
			m.preference = o.value[0]
		case 13:
			if len(o.value) < 2 {
				return m, errors.New("short DHCPv6 status")
			}
			m.status = binary.BigEndian.Uint16(o.value)
		case 23, 31:
			if len(o.value) == 0 || len(o.value)%16 != 0 {
				continue
			}
			for b := o.value; len(b) > 0; b = b[16:] {
				a := netip.AddrFrom16([16]byte(b[:16]))
				if a.Is4In6() || a.IsUnspecified() || a.IsMulticast() {
					continue
				}
				if o.code == 23 {
					m.dns = append(m.dns, a.String())
				} else {
					m.sntp = append(m.sntp, a.String())
				}
			}
		case 24, 64:
			names, e := domainNames(o.value)
			if e != nil {
				continue
			}
			if o.code == 24 {
				m.domains = append(m.domains, names...)
			} else {
				if len(names) != 1 {
					continue
				}
				m.aftr = names[0]
			}
		case 32, 82, 83:
			if len(o.value) != 4 {
				continue
			}
			n := binary.BigEndian.Uint32(o.value)
			switch o.code {
			case 32:
				m.refresh = max(600, n)
			case 82:
				if n >= 60 && n <= 86400 {
					m.maxSolicit = n
				}
			case 83:
				if n >= 60 && n <= 86400 {
					m.maxInformation = n
				}
			}
		}
	}
	return m, nil
}
func decodeIA(kind uint16, b []byte) (association, error) {
	if len(b) < 12 {
		return association{}, errors.New("short DHCPv6 identity association")
	}
	ia := association{iaid: binary.BigEndian.Uint32(b), t1: binary.BigEndian.Uint32(b[4:]), t2: binary.BigEndian.Uint32(b[8:])}
	if ia.t1 != 0 && ia.t2 != 0 && ia.t1 > ia.t2 {
		return ia, errors.New("DHCPv6 T1 exceeds T2")
	}
	opts, err := options(b[12:])
	if err != nil {
		return ia, err
	}
	seen := map[netip.Prefix]bool{}
	for _, o := range opts {
		if o.code == 13 {
			if len(o.value) < 2 {
				return ia, errors.New("short IA status")
			}
			ia.status = binary.BigEndian.Uint16(o.value)
			continue
		}
		var a assignment
		var tail []byte
		switch {
		case kind == 3 && o.code == 5:
			if len(o.value) < 24 {
				return ia, errors.New("short IA Address")
			}
			a = assignment{netip.PrefixFrom(netip.AddrFrom16([16]byte(o.value[:16])), 128), binary.BigEndian.Uint32(o.value[16:]), binary.BigEndian.Uint32(o.value[20:])}
			tail = o.value[24:]
		case kind == 25 && o.code == 26:
			if len(o.value) < 25 || o.value[8] > 128 {
				return ia, errors.New("invalid IA Prefix")
			}
			a = assignment{netip.PrefixFrom(netip.AddrFrom16([16]byte(o.value[9:25])), int(o.value[8])), binary.BigEndian.Uint32(o.value), binary.BigEndian.Uint32(o.value[4:])}
			tail = o.value[25:]
		default:
			continue
		}
		if !usable(a.prefix.Addr()) || a.prefix != a.prefix.Masked() || a.preferred > a.valid || seen[a.prefix] || len(ia.leases) >= 64 {
			return ia, errors.New("invalid/duplicate DHCPv6 assignment")
		}
		sub, e := options(tail)
		if e != nil {
			return ia, e
		}
		for _, s := range sub {
			if s.code == 13 && (len(s.value) < 2 || binary.BigEndian.Uint16(s.value) != 0) {
				return ia, errors.New("failed DHCPv6 assignment status")
			}
		}
		seen[a.prefix] = true
		ia.leases = append(ia.leases, a)
	}
	return ia, nil
}
func usable(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLinkLocalUnicast()
}
func domainNames(b []byte) ([]string, error) {
	var names []string
	for len(b) > 0 {
		var labels []string
		size := 0
		for {
			if len(b) == 0 {
				return nil, errors.New("unterminated DHCPv6 domain name")
			}
			n := int(b[0])
			b = b[1:]
			if n == 0 {
				break
			}
			if n > 63 || n > len(b) || size+n+1 > 253 {
				return nil, errors.New("invalid DHCPv6 domain label")
			}
			for _, ch := range b[:n] {
				if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
					return nil, errors.New("invalid DHCPv6 domain character")
				}
			}
			labels = append(labels, strings.ToLower(string(b[:n])))
			b = b[n:]
			size += n + 1
		}
		if len(labels) == 0 {
			return nil, errors.New("empty DHCPv6 domain")
		}
		names = append(names, strings.Join(labels, "."))
	}
	return names, nil
}
func encodeIA(kind uint16, iaid uint32, leases []assignment) []byte {
	b := binary.BigEndian.AppendUint32(nil, iaid)
	b = append(b, make([]byte, 8)...)
	for _, a := range leases {
		raw := a.prefix.Addr().As16()
		if kind == 3 {
			v := append([]byte(nil), raw[:]...)
			v = append(v, make([]byte, 8)...)
			b = appendOption(b, 5, v)
		} else {
			v := make([]byte, 8)
			v = append(v, byte(a.prefix.Bits()))
			v = append(v, raw[:]...)
			b = appendOption(b, 26, v)
		}
	}
	return b
}
