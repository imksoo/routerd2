//go:build linux

package network

import (
	"fmt"
	"net"
	"sort"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func (k *linuxKernel) PolicyRules() ([]PolicyRuleState, error) {
	// netlink v1.3.1 RuleList does not preserve the fib-rule action or unknown
	// selectors. Decode the dump here so a foreign blackhole/goto/l3mdev rule
	// cannot be mistaken for one of our to-table rules during deletion.
	req := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	messages, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
	if err != nil {
		return nil, err
	}
	var out []PolicyRuleState
	for _, raw := range messages {
		r, err := decodeNativePolicyRule(raw)
		if err != nil {
			return nil, err
		}
		if r.Family == 4 || r.Family == 6 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family == out[j].Family {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Family < out[j].Family
	})
	return out, nil
}

func decodeNativePolicyRule(raw []byte) (PolicyRuleState, error) {
	var out PolicyRuleState
	if len(raw) < unix.SizeofRtMsg {
		return out, fmt.Errorf("truncated policy rule header")
	}
	header := nl.DeserializeRtMsg(raw)
	switch header.Family {
	case unix.AF_INET:
		out.Family = 4
	case unix.AF_INET6:
		out.Family = 6
	default:
		return out, nil
	}
	out.Action = int(header.Type)
	out.Table = int(header.Table)
	out.Invert = header.Flags&netlink.FibRuleInvert != 0
	out.Advanced = header.Flags & ^uint32(netlink.FibRuleInvert) != 0 || header.Tos != 0
	attrs, err := nl.ParseRouteAttr(raw[header.Len():])
	if err != nil {
		return out, err
	}
	order := nl.NativeEndian()
	for _, a := range attrs {
		v := a.Value
		switch a.Attr.Type {
		case nl.FRA_PRIORITY, nl.FRA_TABLE, nl.FRA_FWMARK, nl.FRA_FWMASK, nl.FRA_SUPPRESS_PREFIXLEN, nl.FRA_SUPPRESS_IFGROUP:
			if len(v) != 4 {
				return out, fmt.Errorf("invalid policy rule uint32 attribute %d", a.Attr.Type)
			}
			n := order.Uint32(v)
			switch a.Attr.Type {
			case nl.FRA_PRIORITY:
				out.Priority = int(n)
			case nl.FRA_TABLE:
				out.Table = int(n)
			case nl.FRA_FWMARK:
				out.Mark = &n
			case nl.FRA_FWMASK:
				out.Mask = &n
			default:
				if n != ^uint32(0) {
					out.Advanced = true
				}
			}
		case nl.FRA_SRC, nl.FRA_DST:
			bits := 32
			if out.Family == 6 {
				bits = 128
			}
			prefix := int(header.Src_len)
			if a.Attr.Type == nl.FRA_DST {
				prefix = int(header.Dst_len)
			}
			if len(v) != bits/8 || prefix > bits {
				return out, fmt.Errorf("invalid policy rule prefix")
			}
			s := (&net.IPNet{IP: net.IP(v), Mask: net.CIDRMask(prefix, bits)}).String()
			if a.Attr.Type == nl.FRA_SRC {
				out.Source = s
			} else {
				out.Destination = s
			}
		case nl.FRA_IIFNAME, nl.FRA_OIFNAME:
			if len(v) < 1 || v[len(v)-1] != 0 {
				return out, fmt.Errorf("invalid policy rule interface attribute")
			}
			if a.Attr.Type == nl.FRA_IIFNAME {
				out.InInterface = string(v[:len(v)-1])
			} else {
				out.OutInterface = string(v[:len(v)-1])
			}
		case nl.FRA_PROTOCOL:
			if len(v) != 1 {
				return out, fmt.Errorf("invalid policy rule protocol")
			}
			out.Protocol = int(v[0])
		case nl.FRA_PAD:
		default:
			out.Advanced = true
		}
	}
	if out.Mask != nil && out.Mark == nil {
		mark := uint32(0)
		out.Mark = &mark
	}
	out.PolicyRule = normalizePolicyRule(out.PolicyRule)
	return out, nil
}

func nativePolicyRule(r PolicyRule) (*netlink.Rule, error) {
	out := netlink.NewRule()
	out.Family = unix.AF_INET
	if r.Family == 6 {
		out.Family = unix.AF_INET6
	}
	out.Priority = r.Priority
	out.Table = r.Table
	out.Protocol = RouteProtocol
	out.Type = unix.RTN_UNICAST
	out.IifName = r.InInterface
	out.OifName = r.OutInterface
	out.Invert = r.Invert
	if r.Source != "" {
		_, prefix, err := net.ParseCIDR(r.Source)
		if err != nil {
			return nil, err
		}
		out.Src = prefix
	}
	if r.Destination != "" {
		_, prefix, err := net.ParseCIDR(r.Destination)
		if err != nil {
			return nil, err
		}
		out.Dst = prefix
	}
	if r.Mark != nil {
		out.Mark = *r.Mark
		out.Mask = r.Mask
		if out.Mask == nil {
			mask := ^uint32(0)
			out.Mask = &mask
		}
	}
	return out, nil
}

func (k *linuxKernel) AddPolicyRule(r PolicyRule) error {
	native, err := nativePolicyRule(r)
	if err != nil {
		return err
	}
	return netlink.RuleAdd(native)
}
func (k *linuxKernel) DeletePolicyRule(r PolicyRule) error {
	native, err := nativePolicyRule(r)
	if err != nil {
		return err
	}
	return netlink.RuleDel(native)
}
