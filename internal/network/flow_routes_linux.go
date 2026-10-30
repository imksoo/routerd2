//go:build linux

package network

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

func buildFlowRoutes(c *nftables.Conn, table *nftables.Table, f *Firewall) error {
	if len(f.FlowRoutes) == 0 && len(f.FlowMarks) == 0 {
		return nil
	}
	accept := nftables.ChainPolicyAccept
	pre := c.AddChain(&nftables.Chain{Name: "flow_routes", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-150), Policy: &accept})
	for _, mark := range f.FlowMarks {
		restore := []expr.Any{&expr.Ct{Register: 1, Key: expr.CtKeyDIRECTION}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0}}, &expr.Ct{Register: 1, Key: expr.CtKeyMARK}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)}, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true}}
		c.AddRule(&nftables.Rule{Table: table, Chain: pre, Exprs: restore})
	}
	for i, route := range f.FlowRoutes {
		var buckets *nftables.Set
		if len(route.Buckets) > 1 {
			buckets = &nftables.Set{Table: table, Name: fmt.Sprintf("flow_buckets_%d", i), IsMap: true, KeyType: nftables.TypeInteger, DataType: nftables.TypeMark}
			elements := make([]nftables.SetElement, len(route.Buckets))
			for key, mark := range route.Buckets {
				elements[key] = nftables.SetElement{Key: binaryutil.NativeEndian.PutUint32(uint32(key)), Val: binaryutil.NativeEndian.PutUint32(mark)}
			}
			if err := c.AddSet(buckets, elements); err != nil {
				return err
			}
		}
		for _, match := range packetExpressions(route.Match) {
			for _, value := range route.ExcludeDestinations {
				prefix, _ := netip.ParsePrefix(value)
				ip := prefix.Masked().Addr().As4()
				mask := make([]byte, 4)
				for bit := 0; bit < prefix.Bits(); bit++ {
					mask[bit/8] |= 1 << uint(7-bit%8)
				}
				match = append(match, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4}, &expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: make([]byte, 4)}, &expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ip[:]})
			}
			es := append(match, &expr.Ct{Register: 1, Key: expr.CtKeyDIRECTION}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0}}, &expr.Ct{Register: 1, Key: expr.CtKeyMARK}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: make([]byte, 4)})
			if len(route.Buckets) == 0 {
				es = append(es, &expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(NoPathMark)}, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true}, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictReturn})
				c.AddRule(&nftables.Rule{Table: table, Chain: pre, Exprs: es})
				continue
			}
			if buckets == nil {
				es = append(es, &expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(route.Buckets[0])})
			} else {
				length := uint32(4)
				if route.HashDestination {
					length = 8
				}
				es = append(es, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: length}, &expr.Hash{SourceRegister: 1, DestRegister: 1, Length: length, Modulus: uint32(len(route.Buckets)), Seed: route.Seed, Type: expr.HashTypeJenkins}, &expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: buckets.Name, SetID: buckets.ID})
			}
			es = append(es, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true}, &expr.Ct{Register: 1, SourceRegister: true, Key: expr.CtKeyMARK}, &expr.Counter{})
			c.AddRule(&nftables.Rule{Table: table, Chain: pre, Exprs: es})
		}
	}
	return nil
}
