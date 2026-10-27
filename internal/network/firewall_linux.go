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
	return ownedNftTable(FirewallTable, firewallMarker)
}

func ownedNftTable(name string, marker []byte) (FirewallState, error) {
	s := FirewallState{Table: name}
	c := &nftables.Conn{}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return s, err
	}
	for _, t := range tables {
		if t.Name != name {
			continue
		}
		s.Present = true
		rs, err := c.GetRules(t, &nftables.Chain{Name: ownerChain, Table: t})
		if err != nil {
			return s, nil
		}
		for _, r := range rs {
			if bytes.Equal(r.UserData, marker) {
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
	if err := validateFirewall(f); err != nil {
		return err
	}
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
	if f.Input != nil {
		buildFilterChain(c, t, "input", nftables.ChainHookInput, f.Input, nil)
	}
	if f.Output != nil {
		buildFilterChain(c, t, "output", nftables.ChainHookOutput, f.Output, nil)
	}
	if !f.DisableForward {
		policy := f.ForwardChain
		if policy == nil {
			policy = &FirewallChain{Policy: "drop"}
		}
		buildFilterChain(c, t, "forward", nftables.ChainHookForward, policy, f.Forward)
	}
	if len(f.MSSClamp) > 0 {
		buildMSSClamp(c, t, f.MSSClamp)
	}
	if len(f.IPv4ClearDF) > 0 {
		buildIPv4ClearDF(c, t, f.IPv4ClearDF)
	}
	var pre, post *nftables.Chain
	accept := nftables.ChainPolicyAccept
	for _, rule := range f.NAT {
		chain := post
		if rule.Type == "dnat" {
			if pre == nil {
				pre = c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest, Policy: &accept})
			}
			chain = pre
		} else {
			if post == nil {
				post = c.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept})
			}
			chain = post
		}
		for _, match := range packetExpressions(rule.Match) {
			es := append(match, &expr.Counter{})
			es = append(es, natExpressions(rule)...)
			c.AddRule(&nftables.Rule{Table: t, Chain: chain, Exprs: es})
		}
	}
	if len(f.Masquerade) > 0 && post == nil {
		post = c.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept})
	}
	for _, rule := range f.Masquerade {
		prefix, _ := netip.ParsePrefix(rule.Source)
		es := prefixMatch(prefix)
		es = append(es, interfaceMatch(expr.MetaKeyOIFNAME, rule.OutInterface)...)
		es = append(es, &expr.Counter{}, &expr.Masq{})
		c.AddRule(&nftables.Rule{Table: t, Chain: post, Exprs: es})
	}
}

func buildFilterChain(c *nftables.Conn, table *nftables.Table, name string, hook *nftables.ChainHook, config *FirewallChain, pairs []Forward) {
	policy, verdict := nftables.ChainPolicyDrop, expr.VerdictDrop
	if config.Policy == "accept" {
		policy, verdict = nftables.ChainPolicyAccept, expr.VerdictAccept
	}
	chain := c.AddChain(&nftables.Chain{Name: name, Table: table, Type: nftables.ChainTypeFilter, Hooknum: hook, Priority: nftables.ChainPriorityFilter, Policy: &policy})
	add := func(es []expr.Any) { c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: es}) }
	add(stateVerdict(expr.CtStateBitINVALID, expr.VerdictDrop))
	add(stateVerdict(expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED, expr.VerdictAccept))
	for _, rule := range config.Rules {
		for _, es := range packetExpressions(rule.Match) {
			es = append(es, logExpressions(rule.LogPrefix)...)
			es = append(es, &expr.Counter{})
			switch rule.Action {
			case "accept":
				es = append(es, &expr.Verdict{Kind: expr.VerdictAccept})
			case "drop":
				es = append(es, &expr.Verdict{Kind: expr.VerdictDrop})
			case "reject":
				es = append(es, &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_PORT_UNREACH})
			}
			add(es)
		}
	}
	for _, pair := range pairs {
		es := interfaceMatch(expr.MetaKeyIIFNAME, pair.From)
		es = append(es, interfaceMatch(expr.MetaKeyOIFNAME, pair.To)...)
		es = append(es, &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictAccept})
		add(es)
	}
	es := logExpressions(config.LogPrefix)
	es = append(es, &expr.Counter{}, &expr.Verdict{Kind: verdict})
	add(es)
}

func logExpressions(prefix string) []expr.Any {
	if prefix == "" {
		return nil
	}
	return []expr.Any{&expr.Log{Key: 1<<unix.NFTA_LOG_PREFIX | 1<<unix.NFTA_LOG_LEVEL, Level: expr.LogLevelInfo, Data: []byte(prefix)}}
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
	return prefixFieldMatch(prefix, true)
}
func prefixFieldMatch(prefix netip.Prefix, source bool) []expr.Any {
	proto, offset, length := byte(unix.NFPROTO_IPV6), uint32(8), uint32(16)
	if prefix.Addr().Is4() {
		proto, offset, length = unix.NFPROTO_IPV4, 12, 4
	}
	if !source {
		offset += length
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

// Like routerd's mangle rule, rewrite only oversized MSS options on SYN without
// RST. TCP option writes let the kernel update the checksum; no packet is rebuilt.
func buildMSSClamp(c *nftables.Conn, table *nftables.Table, rules []MSSClamp) {
	accept := nftables.ChainPolicyAccept
	chain := c.AddChain(&nftables.Chain{Name: "mss_clamp", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityMangle, Policy: &accept})
	for _, rule := range rules {
		header := 40
		if rule.Family == 6 {
			header = 60
		}
		mss := binaryutil.BigEndian.PutUint16(uint16(rule.MTU - header))
		for _, match := range packetExpressions(PacketMatch{Family: rule.Family, InInterface: rule.From, OutInterface: rule.To, Protocol: "tcp"}) {
			match = append(match,
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
				&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x06}, Xor: []byte{0}},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x02}},
				&expr.Exthdr{DestRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
				&expr.Cmp{Op: expr.CmpOpGt, Register: 1, Data: mss},
				&expr.Immediate{Register: 1, Data: mss},
				&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
				&expr.Counter{},
			)
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: match})
		}
	}
}

// IPv4 checks DF/MTU before the forward hook. Match after DNAT so the FIB sees
// the translated destination, and include packet marks used by policy routing.
func buildIPv4ClearDF(c *nftables.Conn, table *nftables.Table, rules []IPv4ClearDF) {
	accept := nftables.ChainPolicyAccept
	chain := c.AddChain(&nftables.Chain{Name: "ipv4_clear_df", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-99), Policy: &accept})
	for _, rule := range rules {
		for _, match := range packetExpressions(PacketMatch{Family: 4, InInterface: rule.From}) {
			name := make([]byte, 16)
			copy(name, rule.To)
			match = append(match,
				&expr.Fib{Register: 1, ResultOIFNAME: true, FlagDADDR: true, FlagMARK: true},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpGt, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(rule.MTU))},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 6, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x40, 0}},
				&expr.Immediate{Register: 1, Data: []byte{0, 0}},
				&expr.Payload{OperationType: expr.PayloadWrite, SourceRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 6, Len: 2, CsumType: expr.CsumTypeInet, CsumOffset: 10},
				&expr.Counter{},
			)
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: match})
		}
	}
}
