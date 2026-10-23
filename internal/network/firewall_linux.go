//go:build linux

package network

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const ownerChain = "ownership"

var firewallMarker = []byte("routerd2:owned-table:v1")

func (k *linuxKernel) Firewall() (FirewallState, error) {
	s := FirewallState{Table: FirewallTable}
	c := &nftables.Conn{}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return s, err
	}
	for _, t := range tables {
		if t.Name != FirewallTable {
			continue
		}
		s.Present = true
		rs, err := c.GetRules(t, &nftables.Chain{Name: ownerChain, Table: t})
		if err != nil {
			return s, nil
		}
		for _, r := range rs {
			if bytes.Equal(r.UserData, firewallMarker) {
				s.Owned = true
				break
			}
		}
		break
	}
	return s, nil
}

// ReplaceFirewall queues deletion and recreation in one nftables batch. No
// FlushRuleset or other table's operation is ever queued. Conntrack survives.
func (k *linuxKernel) ReplaceFirewall(f *Firewall) error {
	c := &nftables.Conn{}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if t.Name == FirewallTable {
			c.DelTable(t)
			break
		}
	}
	if f != nil {
		buildFirewall(c, f)
	}
	return c.Flush()
}

func buildFirewall(c *nftables.Conn, f *Firewall) {
	t := c.CreateTable(&nftables.Table{Name: FirewallTable, Family: nftables.TableFamilyINet})
	marker := c.AddChain(&nftables.Chain{Name: ownerChain, Table: t})
	c.AddRule(&nftables.Rule{Table: t, Chain: marker, Exprs: []expr.Any{&expr.Counter{}}, UserData: firewallMarker})
	drop := nftables.ChainPolicyDrop
	forward := c.AddChain(&nftables.Chain{Name: "forward", Table: t, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	add := func(expressions []expr.Any) { c.AddRule(&nftables.Rule{Table: t, Chain: forward, Exprs: expressions}) }
	add(stateVerdict(expr.CtStateBitINVALID, expr.VerdictDrop))
	add(stateVerdict(expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED, expr.VerdictAccept))
	for _, pair := range f.Forward {
		es := interfaceMatch(expr.MetaKeyIIFNAME, pair.From)
		es = append(es, interfaceMatch(expr.MetaKeyOIFNAME, pair.To)...)
		es = append(es, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictAccept})
		add(es)
	}
	if len(f.Masquerade) == 0 {
		return
	}
	accept := nftables.ChainPolicyAccept
	nat := c.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept})
	for _, rule := range f.Masquerade {
		prefix, _ := netip.ParsePrefix(rule.Source)
		es := prefixMatch(prefix)
		es = append(es, interfaceMatch(expr.MetaKeyOIFNAME, rule.OutInterface)...)
		es = append(es, &expr.Counter{}, &expr.Masq{})
		c.AddRule(&nftables.Rule{Table: t, Chain: nat, Exprs: es})
	}
}

func stateVerdict(mask uint32, verdict expr.VerdictKind) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(mask), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
		&expr.Counter{}, &expr.Verdict{Kind: verdict},
	}
}
func interfaceMatch(key expr.MetaKey, name string) []expr.Any {
	b := make([]byte, 16)
	copy(b, name)
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b}}
}
func prefixMatch(prefix netip.Prefix) []expr.Any {
	proto, offset, length := byte(unix.NFPROTO_IPV6), uint32(8), uint32(16)
	if prefix.Addr().Is4() {
		proto, offset, length = unix.NFPROTO_IPV4, 12, 4
	}
	mask := []byte(net.CIDRMask(prefix.Bits(), int(length)*8))
	address := prefix.Masked().Addr().AsSlice()
	if len(mask) != int(length) || len(address) != int(length) {
		panic(fmt.Sprintf("invalid validated prefix %s", prefix))
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: length},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: length, Mask: mask, Xor: make([]byte, length)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: address},
	}
}
