//go:build linux

package network

import (
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

var vrrpGuardMarker = []byte("routerd2:owned-ha-table:v1")

func (k *linuxKernel) VRRPGuard() (FirewallState, error) {
	return ownedNftTable(VRRPGuardTable, vrrpGuardMarker)
}

func (k *linuxKernel) ReplaceVRRPGuard(guards []VRRPGuard) error {
	c := &nftables.Conn{}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if table.Name == VRRPGuardTable {
			c.DelTable(table)
			break
		}
	}
	if len(guards) > 0 {
		buildVRRPGuards(c, guards)
	}
	return c.Flush()
}

func buildVRRPGuards(c *nftables.Conn, guards []VRRPGuard) {
	t := c.CreateTable(&nftables.Table{Name: VRRPGuardTable, Family: nftables.TableFamilyINet})
	marker := c.AddChain(&nftables.Chain{Name: ownerChain, Table: t})
	c.AddRule(&nftables.Rule{Table: t, Chain: marker, Exprs: []expr.Any{&expr.Counter{}}, UserData: vrrpGuardMarker})
	accept := nftables.ChainPolicyAccept
	input := c.AddChain(&nftables.Chain{Name: "input", Table: t, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &accept})
	for _, guard := range guards {
		for _, s := range guard.Addresses {
			address, _ := netip.ParseAddr(s)
			add := func(tail []expr.Any) {
				expressions := destinationAddressMatch(address)
				expressions = append(expressions, tail...)
				c.AddRule(&nftables.Rule{Table: t, Chain: input, Exprs: expressions})
			}
			add([]expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{112}}, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictAccept}})
			if address.Is6() {
				for _, kind := range []byte{135, 136} {
					add([]expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}}, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{kind}}, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictAccept}})
				}
			}
			add([]expr.Any{&expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop}})
		}
	}
}

func destinationAddressMatch(address netip.Addr) []expr.Any {
	proto, offset, length := byte(unix.NFPROTO_IPV6), uint32(24), uint32(16)
	if address.Is4() {
		proto, offset, length = unix.NFPROTO_IPV4, 16, 4
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: length}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: address.AsSlice()},
	}
}
