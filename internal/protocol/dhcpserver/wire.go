// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

const (
	msgDiscover byte = 1
	msgOffer    byte = 2
	msgRequest  byte = 3
	msgDecline  byte = 4
	msgACK      byte = 5
	msgNAK      byte = 6
	msgRelease  byte = 7
	msgInform   byte = 8
)

var zero4 = netip.AddrFrom4([4]byte{})
var broadcast4 = netip.AddrFrom4([4]byte{255, 255, 255, 255})
var broadcastMAC = net.HardwareAddr{255, 255, 255, 255, 255, 255}

type message struct {
	Type                           byte
	XID                            uint32
	Flags                          uint16
	Hops                           byte
	CIAddr, YIAddr, SIAddr, GIAddr netip.Addr
	MAC                            net.HardwareAddr
	Options                        map[byte][]byte
}

// decodeRequest is deliberately independent of the original permissive client
// helper. Every field is bounded before slicing; RFC 3396 option fragments are
// concatenated, after which singleton options must have their exact length.
func decodeRequest(b []byte) (message, error) {
	if len(b) < 240 || len(b) > 4096 {
		return message{}, errors.New("DHCP packet size outside 240..4096")
	}
	if b[0] != 1 || b[1] != 1 || b[2] != 6 || b[3] > 16 {
		return message{}, errors.New("DHCP request requires Ethernet BOOTREQUEST with bounded hops")
	}
	if string(b[236:240]) != string([]byte{99, 130, 83, 99}) {
		return message{}, errors.New("invalid DHCP cookie")
	}
	m := message{XID: binary.BigEndian.Uint32(b[4:8]), Flags: binary.BigEndian.Uint16(b[10:12]), Hops: b[3], CIAddr: addr4(b[12:16]), YIAddr: addr4(b[16:20]), SIAddr: addr4(b[20:24]), GIAddr: addr4(b[24:28]), MAC: append(net.HardwareAddr(nil), b[28:34]...), Options: map[byte][]byte{}}
	// RFC 2131 requires servers to ignore the fifteen reserved flag bits.
	m.Flags &= 0x8000
	if !m.YIAddr.IsUnspecified() || !m.SIAddr.IsUnspecified() {
		return message{}, errors.New("client yiaddr/siaddr must be zero")
	}
	if err := decodeOptions(b[240:], m.Options, false); err != nil {
		return message{}, err
	}
	if overload, ok := m.Options[52]; ok {
		if len(overload) != 1 || overload[0] < 1 || overload[0] > 3 {
			return message{}, errors.New("invalid option overload")
		}
		if overload[0]&1 != 0 {
			if err := decodeOptions(b[108:236], m.Options, true); err != nil {
				return message{}, err
			}
		}
		if overload[0]&2 != 0 {
			if err := decodeOptions(b[44:108], m.Options, true); err != nil {
				return message{}, err
			}
		}
	}
	for code, n := range map[byte]int{50: 4, 51: 4, 52: 1, 53: 1, 54: 4, 57: 2, 58: 4, 59: 4} {
		if v, ok := m.Options[code]; ok && len(v) != n {
			return message{}, errors.New("invalid singleton DHCP option length")
		}
	}
	for _, code := range []byte{50, 54} {
		if v, ok := m.Options[code]; ok {
			a := addr4(v)
			if _, err := dhcpserverconfig.IPv4(a.String()); err != nil {
				return message{}, errors.New("invalid requested address or server identifier")
			}
		}
	}
	if len(m.Options[53]) != 1 {
		return message{}, errors.New("DHCP message type is required")
	}
	m.Type = m.Options[53][0]
	switch m.Type {
	case msgDiscover, msgRequest, msgDecline, msgRelease, msgInform:
	default:
		return message{}, errors.New("unsupported client DHCP message type")
	}
	if v, ok := m.Options[61]; ok && (len(v) < 2 || len(v) > 255) {
		return message{}, errors.New("invalid DHCP client identifier")
	}
	if v, ok := m.Options[57]; ok && binary.BigEndian.Uint16(v) < 576 {
		return message{}, errors.New("maximum DHCP message size below 576")
	}
	if m.MAC[0]&1 != 0 {
		return message{}, errors.New("multicast client hardware address")
	}
	if string(m.MAC) == string(make([]byte, 6)) {
		if len(m.Options[61]) == 0 || m.GIAddr.IsUnspecified() && m.CIAddr.IsUnspecified() && m.Flags&0x8000 == 0 {
			return message{}, errors.New("zero chaddr requires client identifier and an addressable reply")
		}
	}
	if _, ok := m.Options[90]; ok {
		return message{}, errors.New("DHCP authentication is not implemented")
	}
	if _, ok := m.Options[80]; ok {
		return message{}, errors.New("DHCP rapid commit is not implemented")
	}
	return m, nil
}
func decodeOptions(b []byte, out map[byte][]byte, overloaded bool) error {
	for i := 0; i < len(b); {
		code := b[i]
		i++
		if code == 0 {
			continue
		}
		if code == 255 {
			return nil
		}
		if i >= len(b) {
			return errors.New("truncated DHCP option length")
		}
		n := int(b[i])
		i++
		if n > len(b)-i {
			return errors.New("truncated DHCP option value")
		}
		if overloaded && code == 52 {
			return errors.New("nested DHCP option overload")
		}
		if len(out[code])+n > 2048 {
			return errors.New("DHCP option concatenation exceeds bound")
		}
		out[code] = append(out[code], b[i:i+n]...)
		i += n
	}
	return errors.New("DHCP options have no end marker")
}
func addr4(b []byte) netip.Addr { var a [4]byte; copy(a[:], b); return netip.AddrFrom4(a) }
func optionAddress(m message, code byte) netip.Addr {
	if b := m.Options[code]; len(b) == 4 {
		return addr4(b)
	}
	return zero4
}
func putAddress(dst []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(dst, v[:])
	}
}
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

func replyOptions(c Config, s Scope, r *Reservation, m message, typ byte, leaseSeconds uint32) (map[byte][]byte, error) {
	options := map[byte][]byte{53: {typ}, 54: append([]byte(nil), net.ParseIP(c.ServerAddress).To4()...)}
	if id := m.Options[61]; len(id) > 0 {
		options[61] = append([]byte(nil), id...)
	}
	if relay := m.Options[82]; len(relay) > 0 {
		options[82] = append([]byte(nil), relay...)
	}
	if typ == msgNAK {
		return options, nil
	}
	p, _ := netip.ParsePrefix(s.Prefix)
	mask := uint32(0xffffffff) << (32 - p.Bits())
	options[1] = u32(mask)
	if m.Type != msgInform {
		options[51] = u32(leaseSeconds)
		options[58] = u32(leaseSeconds / 2)
		options[59] = u32(uint32(uint64(leaseSeconds) * 7 / 8))
	}
	if s.Router != "" {
		a, _ := netip.ParseAddr(s.Router)
		v := a.As4()
		options[3] = append([]byte(nil), v[:]...)
	}
	for code, list := range map[byte][]string{6: s.DNSServers, 42: s.NTPServers} {
		for _, v := range list {
			a, _ := netip.ParseAddr(v)
			ip := a.As4()
			options[code] = append(options[code], ip[:]...)
		}
	}
	if s.Domain != "" {
		options[15] = []byte(strings.TrimSuffix(s.Domain, "."))
		encoded, err := dhcpserverconfig.EncodeOptions([]dhcpserverconfig.Option{{Code: 119, Value: s.Domain}})
		if err != nil {
			return nil, err
		}
		options[119] = encoded[119]
	}
	for _, set := range [][]dhcpserverconfig.Option{s.Options, func() []dhcpserverconfig.Option {
		if r != nil {
			return r.Options
		}
		return nil
	}()} {
		extra, err := dhcpserverconfig.EncodeOptions(set)
		if err != nil {
			return nil, err
		}
		for code, value := range extra {
			if len(value) == 0 {
				delete(options, code)
			} else {
				options[code] = value
			}
		}
	}
	if r != nil && r.Hostname != "" {
		options[12] = []byte(r.Hostname)
	}
	return options, nil
}

// encodeReply includes protocol-critical options first, then respects the
// client's parameter ordering. RFC 3396 long values use multiple TLVs. Optional
// values that cannot fit the client's size/MTU bound are omitted as whole values.
func encodeReply(m message, typ byte, yi, server netip.Addr, options map[byte][]byte, mtu int) ([]byte, error) {
	limit := min(1472, mtu-28)
	clientLimit := 576 - 28
	if x := m.Options[57]; len(x) == 2 {
		// Option 57 includes IPv4 and UDP headers (RFC 3442).
		clientLimit = int(binary.BigEndian.Uint16(x)) - 28
	}
	limit = min(limit, clientLimit)
	if limit < 300 {
		return nil, errors.New("DHCP reply interface MTU is too small")
	}
	b := make([]byte, 240)
	b[0], b[1], b[2] = 2, 1, 6
	binary.BigEndian.PutUint32(b[4:8], m.XID)
	flags := m.Flags
	if typ == msgNAK && !m.GIAddr.IsUnspecified() {
		flags |= 0x8000
	}
	binary.BigEndian.PutUint16(b[10:12], flags)
	if typ == msgACK {
		putAddress(b[12:16], m.CIAddr)
	}
	if typ != msgNAK && m.Type != msgInform {
		putAddress(b[16:20], yi)
	}
	putAddress(b[24:28], m.GIAddr)
	copy(b[28:44], m.MAC)
	copy(b[236:240], []byte{99, 130, 83, 99})
	appendOption := func(code byte, required bool) error {
		v, ok := options[code]
		if !ok {
			return nil
		}
		n := len(v) + 2*((len(v)+254)/255)
		if len(v) == 0 {
			n = 2
		}
		if len(b)+n+1 > limit {
			if required {
				return errors.New("mandatory DHCP options exceed client message size")
			}
			return nil
		}
		if len(v) == 0 {
			b = append(b, code, 0)
			return nil
		}
		for len(v) > 0 {
			part := min(255, len(v))
			b = append(b, code, byte(part))
			b = append(b, v[:part]...)
			v = v[part:]
		}
		return nil
	}
	used := map[byte]bool{}
	for _, code := range []byte{53, 54, 61, 82, 1, 51, 58, 59} {
		if err := appendOption(code, true); err != nil {
			return nil, err
		}
		used[code] = true
	}
	requested := m.Options[55]
	var order []byte
	if len(requested) > 0 {
		order = append(order, requested...)
	} else {
		for code := range options {
			order = append(order, code)
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	}
	// Router is omitted only if classless routes are actually selected for
	// this client, as required by RFC 3442.
	classless := false
	for _, code := range order {
		if code == 121 && len(options[121]) > 0 {
			before := len(b)
			if err := appendOption(121, false); err != nil {
				return nil, err
			}
			classless = len(b) > before
			used[121] = true
			break
		}
	}
	for _, code := range order {
		if used[code] || classless && (code == 3 || code == 33) {
			continue
		}
		used[code] = true
		if err := appendOption(code, false); err != nil {
			return nil, err
		}
	}
	b = append(b, 255)
	for len(b) < 300 {
		b = append(b, 0)
	}
	return b, nil
}

func makeResponse(m message, typ byte, data []byte, yi netip.Addr) response {
	r := response{Data: data, Port: 68}
	switch {
	case !m.GIAddr.IsUnspecified():
		r.Destination = m.GIAddr
		r.Port = 67
		r.Routed = true
	case typ == msgNAK:
		r.Destination = broadcast4
		r.MAC = append(net.HardwareAddr(nil), broadcastMAC...)
	case !m.CIAddr.IsUnspecified():
		r.Destination = m.CIAddr
		r.Routed = true
	case m.Flags&0x8000 != 0:
		r.Destination = broadcast4
		r.MAC = append(net.HardwareAddr(nil), broadcastMAC...)
	default:
		r.Destination = yi
		r.MAC = append(net.HardwareAddr(nil), m.MAC...)
	}
	return r
}
