//go:build linux

package network

import (
	"fmt"
	"net"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// v1.3.1's RouteList discards RTA_CACHEINFO/RTA_EXPIRES and unknown route
// selectors. Preserve them here: a finite foreign route must never look like
// an owned static route, and lifetime drift must not authorize its deletion.
func nativeRouteDump() ([][]byte, error) {
	req := nl.NewNetlinkRequest(unix.RTM_GETROUTE, unix.NLM_F_DUMP)
	req.AddData(&nl.RtMsg{RtMsg: unix.RtMsg{Family: unix.AF_UNSPEC}})
	return req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWROUTE)
}

func decodeNativeRoute(raw []byte, names map[int]string, now routeClock) (RouteState, error) {
	if len(raw) < unix.SizeofRtMsg {
		return RouteState{}, fmt.Errorf("truncated route header")
	}
	h := nl.DeserializeRtMsg(raw)
	if h.Family != unix.AF_INET && h.Family != unix.AF_INET6 || h.Flags&unix.RTM_F_CLONED != 0 {
		return RouteState{}, nil
	}
	bits := 32
	if h.Family == unix.AF_INET6 {
		bits = 128
	}
	if int(h.Dst_len) > bits || int(h.Src_len) > bits {
		return RouteState{}, fmt.Errorf("invalid route prefix length")
	}
	r := netlink.Route{Family: int(h.Family), Table: int(h.Table), Protocol: netlink.RouteProtocol(h.Protocol), Type: int(h.Type), Scope: netlink.Scope(h.Scope), Tos: int(h.Tos), Flags: int(h.Flags)}
	attrs, err := nl.ParseRouteAttr(raw[h.Len():])
	if err != nil {
		return RouteState{}, err
	}
	var duration time.Duration
	advanced := h.Src_len != 0
	seen := map[uint16]bool{}
	order := nl.NativeEndian()
	for _, a := range attrs {
		kind, v := a.Attr.Type, a.Value
		if seen[kind] {
			return RouteState{}, fmt.Errorf("duplicate route attribute %d", kind)
		}
		seen[kind] = true
		switch kind {
		case unix.RTA_DST, unix.RTA_GATEWAY, unix.RTA_PREFSRC:
			if len(v) != bits/8 {
				return RouteState{}, fmt.Errorf("invalid route IP attribute %d", kind)
			}
			switch kind {
			case unix.RTA_DST:
				r.Dst = &net.IPNet{IP: net.IP(v), Mask: net.CIDRMask(int(h.Dst_len), bits)}
			case unix.RTA_GATEWAY:
				r.Gw = net.IP(v)
			case unix.RTA_PREFSRC:
				r.Src = net.IP(v)
			}
		case unix.RTA_OIF, unix.RTA_IIF, unix.RTA_TABLE, unix.RTA_PRIORITY, unix.RTA_EXPIRES:
			if len(v) != 4 {
				return RouteState{}, fmt.Errorf("invalid route integer attribute %d", kind)
			}
			n := order.Uint32(v)
			switch kind {
			case unix.RTA_OIF:
				r.LinkIndex = int(n)
			case unix.RTA_IIF:
				r.ILinkIndex = int(n)
			case unix.RTA_TABLE:
				r.Table = int(n)
			case unix.RTA_PRIORITY:
				r.Priority = int(n)
			case unix.RTA_EXPIRES:
				duration = (time.Duration(n) * time.Second)
			}
		case unix.RTA_CACHEINFO:
			if len(v) != 32 {
				return RouteState{}, fmt.Errorf("invalid route cache info")
			}
			// Linux's ABI uses signed USER_HZ ticks here, rather than seconds.
			// USER_HZ is 100 on the supported Linux amd64/arm64 targets.
			if ticks := int32(order.Uint32(v[8:12])); ticks != 0 {
				duration = (time.Duration(ticks) * (time.Second / 100))
			}
			advanced = advanced || order.Uint32(v[12:16]) != 0
		case unix.RTA_MULTIPATH:
			for len(v) > 0 {
				if len(v) < unix.SizeofRtNexthop {
					return RouteState{}, fmt.Errorf("truncated route next hop")
				}
				nh := nl.DeserializeRtNexthop(v)
				length := int(nh.RtNexthop.Len)
				aligned := (length + 3) &^ 3
				if length < unix.SizeofRtNexthop || length > len(v) || aligned > len(v) {
					return RouteState{}, fmt.Errorf("invalid route next-hop length")
				}
				next := &netlink.NexthopInfo{LinkIndex: int(nh.Ifindex), Hops: int(nh.Hops), Flags: int(nh.Flags)}
				children, err := nl.ParseRouteAttr(v[unix.SizeofRtNexthop:length])
				if err != nil {
					return RouteState{}, err
				}
				for _, child := range children {
					if child.Attr.Type == unix.RTA_GATEWAY {
						if len(child.Value) != bits/8 || next.Gw != nil {
							return RouteState{}, fmt.Errorf("invalid next-hop gateway")
						}
						next.Gw = net.IP(child.Value)
					} else {
						advanced = true
					}
				}
				r.MultiPath = append(r.MultiPath, next)
				v = v[aligned:]
			}
		case unix.RTA_PREF:
			// Kernel dumps the default IPv6 medium preference as zero. Other
			// preferences are not set by this backend and remain foreign.
			if len(v) != 1 {
				return RouteState{}, fmt.Errorf("invalid IPv6 route preference")
			}
			advanced = advanced || v[0] != 0
		case unix.RTA_PAD:
		default:
			advanced = true
		}
	}
	if r.Dst == nil && h.Dst_len != 0 {
		return RouteState{}, fmt.Errorf("route prefix has no address")
	}
	out := nativeRouteState(r, names)
	if duration != 0 {
		out.ValidUntil = lifetime.Deadline{UTC: now.WallTime.Add(duration)}
		out.kernelExpiry = routeTimer{BootID: now.BootID, Nanos: uint64(max(0, now.monotonicNanos+int64(duration)))}
	}
	out.Advanced = out.Advanced || advanced
	// IPv6 always dumps universe scope; IPv4 preserves the configured scope.
	expectedScope := uint8(unix.RT_SCOPE_UNIVERSE)
	if h.Family == unix.AF_INET && len(r.Gw) == 0 {
		expectedScope = unix.RT_SCOPE_LINK
		for _, next := range r.MultiPath {
			if len(next.Gw) != 0 {
				expectedScope = unix.RT_SCOPE_UNIVERSE
			}
		}
	}
	out.Advanced = out.Advanced || h.Scope != expectedScope
	return out, nil
}

func expiringRouteRequest(r *netlink.Route, remaining time.Duration) (*nl.NetlinkRequest, error) {
	if r.Family != unix.AF_INET6 || remaining <= 0 || remaining > maxRouteLifetime {
		return nil, fmt.Errorf("invalid or expired IPv6 route lifetime")
	}
	h := nl.NewRtMsg()
	h.Family, h.Protocol, h.Scope, h.Type = unix.AF_INET6, uint8(r.Protocol), uint8(r.Scope), uint8(r.Type)
	bits, _ := r.Dst.Mask.Size()
	h.Dst_len = uint8(bits)
	h.Table = uint8(r.Table)
	if r.Table > 255 {
		h.Table = unix.RT_TABLE_UNSPEC
	}
	req := nl.NewNetlinkRequest(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	req.AddData(h)
	req.AddData(nl.NewRtAttr(unix.RTA_DST, r.Dst.IP.To16()))
	req.AddData(nl.NewRtAttr(unix.RTA_TABLE, nl.Uint32Attr(uint32(r.Table))))
	if r.Src != nil {
		req.AddData(nl.NewRtAttr(unix.RTA_PREFSRC, r.Src.To16()))
	}
	req.AddData(nl.NewRtAttr(unix.RTA_PRIORITY, nl.Uint32Attr(uint32(r.Priority))))
	req.AddData(nl.NewRtAttr(unix.RTA_EXPIRES, nl.Uint32Attr(uint32((remaining+time.Second-1)/time.Second))))
	if len(r.MultiPath) > 0 {
		var data []byte
		for _, next := range r.MultiPath {
			nh := &nl.RtNexthop{RtNexthop: unix.RtNexthop{Ifindex: int32(next.LinkIndex), Hops: uint8(next.Hops)}}
			if next.Gw != nil {
				nh.Children = append(nh.Children, nl.NewRtAttr(unix.RTA_GATEWAY, next.Gw.To16()))
			}
			data = append(data, nh.Serialize()...)
		}
		req.AddData(nl.NewRtAttr(unix.RTA_MULTIPATH, data))
	} else {
		req.AddData(nl.NewRtAttr(unix.RTA_OIF, nl.Uint32Attr(uint32(r.LinkIndex))))
		if r.Gw != nil {
			req.AddData(nl.NewRtAttr(unix.RTA_GATEWAY, r.Gw.To16()))
		}
	}
	return req, nil
}
