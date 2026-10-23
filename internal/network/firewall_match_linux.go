//go:build linux

package network

import (
	"net/netip"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

func packetExpressions(m PacketMatch) [][]expr.Any {
	var base []expr.Any
	if family := matchFamily(m); family != 0 {
		proto := byte(unix.NFPROTO_IPV6)
		if family == 4 {
			proto = unix.NFPROTO_IPV4
		}
		base = append(base, &expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}})
	}
	if m.InInterface != "" {
		base = append(base, interfaceMatch(expr.MetaKeyIIFNAME, m.InInterface)...)
	}
	if m.OutInterface != "" {
		base = append(base, interfaceMatch(expr.MetaKeyOIFNAME, m.OutInterface)...)
	}
	if m.Source != "" {
		p, _ := netip.ParsePrefix(m.Source)
		base = append(base, prefixFieldMatch(p, true)...)
	}
	if m.Destination != "" {
		p, _ := netip.ParsePrefix(m.Destination)
		base = append(base, prefixFieldMatch(p, false)...)
	}
	if m.Protocol != "" {
		proto := map[string]byte{"tcp": unix.IPPROTO_TCP, "udp": unix.IPPROTO_UDP, "icmp": unix.IPPROTO_ICMP, "icmpv6": unix.IPPROTO_ICMPV6, "ipv6-icmp": unix.IPPROTO_ICMPV6, "ipip": unix.IPPROTO_IPIP, "vrrp": 112}[m.Protocol]
		base = append(base, &expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}})
	}
	source, destination, kinds := m.SourcePorts, m.DestinationPorts, m.ICMPTypes
	if len(source) == 0 {
		source = []PortRange{{}}
	}
	if len(destination) == 0 {
		destination = []PortRange{{}}
	}
	if len(kinds) == 0 {
		kinds = []int{-1}
	}
	var result [][]expr.Any
	for _, s := range source {
		for _, d := range destination {
			for _, kind := range kinds {
				es := append([]expr.Any(nil), base...)
				if s.Start != 0 {
					es = append(es, portExpressions(s, 0)...)
				}
				if d.Start != 0 {
					es = append(es, portExpressions(d, 2)...)
				}
				if kind >= 0 {
					es = append(es, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(kind)}})
				}
				result = append(result, es)
			}
		}
	}
	return result
}

func portExpressions(ports PortRange, offset uint32) []expr.Any {
	p := normalizedPortRange(ports)
	es := []expr.Any{&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: offset, Len: 2}}
	if p.Start == p.End {
		return append(es, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(p.Start))})
	}
	return append(es, &expr.Cmp{Op: expr.CmpOpGte, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(p.Start))}, &expr.Cmp{Op: expr.CmpOpLte, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(p.End))})
}

func natExpressions(rule NATRule) []expr.Any {
	var es []expr.Any
	var portMin, portMax uint32
	if rule.Ports != nil {
		p := normalizedPortRange(*rule.Ports)
		es = append(es, &expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(uint16(p.Start))})
		portMin = 2
		if p.End != p.Start {
			es = append(es, &expr.Immediate{Register: 3, Data: binaryutil.BigEndian.PutUint16(uint16(p.End))})
			portMax = 3
		}
	}
	if rule.Type == "masquerade" {
		return append(es, &expr.Masq{ToPorts: rule.Ports != nil, RegProtoMin: portMin, RegProtoMax: portMax})
	}
	address, _ := netip.ParseAddr(rule.Address)
	es = append(es, &expr.Immediate{Register: 1, Data: address.AsSlice()})
	family := uint32(unix.NFPROTO_IPV6)
	if rule.Match.Family == 4 {
		family = unix.NFPROTO_IPV4
	}
	typeValue := expr.NATTypeSourceNAT
	if rule.Type == "dnat" {
		typeValue = expr.NATTypeDestNAT
	}
	return append(es, &expr.NAT{Type: typeValue, Family: family, RegAddrMin: 1, RegProtoMin: portMin, RegProtoMax: portMax})
}
