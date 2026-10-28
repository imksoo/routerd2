//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type linuxKernel struct{}

func newKernel() (kernel, error)   { return &linuxKernel{}, nil }
func lockJournal(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func linuxLink(name string) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("%s: %w", name, errLinkNotFound)
	}
	return l, err
}

func (k *linuxKernel) Interface(name string) (InterfaceState, error) {
	l, err := linuxLink(name)
	if err != nil {
		return InterfaceState{}, err
	}
	out, err := nativeLinkState(l)
	if err != nil {
		return out, err
	}
	if value, readErr := k.ReadSetting(settingKey{Kind: "accept_ra", Interface: name}); readErr == nil {
		out.IPv6AcceptRA = &value
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return out, readErr
	}
	if value, readErr := k.ReadSetting(settingKey{Kind: "autoconf", Interface: name}); readErr == nil {
		enabled := value != 0
		out.IPv6Autoconf = &enabled
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return out, readErr
	}
	as, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return out, err
	}
	for _, addr := range as {
		if addr.IPNet != nil {
			peer := ""
			if addr.Peer != nil && !addr.Peer.IP.Equal(addr.IP) {
				peer = addr.Peer.String()
			}
			out.Addresses = append(out.Addresses, AddressState{Address: addr.IPNet.String(), Peer: peer, Flags: addr.Flags, PreferredLifetime: uint32(addr.PreferedLft), ValidLifetime: uint32(addr.ValidLft)})
		}
	}
	sort.Slice(out.Addresses, func(i, j int) bool { return out.Addresses[i].Address < out.Addresses[j].Address })
	return out, nil
}

func nativeAddressIdentity(address string, options AddressOptions) (*netlink.Addr, error) {
	a, err := netlink.ParseAddr(address)
	if err != nil {
		return nil, err
	}
	if options.Peer != "" {
		peer, err := netlink.ParseAddr(options.Peer)
		if err != nil {
			return nil, err
		}
		a.Peer = peer.IPNet
	}
	return a, nil
}

func nativeAddress(address string, options AddressOptions) (*netlink.Addr, error) {
	a, err := nativeAddressIdentity(address, options)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	preferred, valid, err := addressLifetimes(options, now)
	if err != nil {
		return nil, err
	}
	if valid == 0 {
		return nil, fmt.Errorf("address %s valid lifetime has expired", address)
	}
	a.PreferedLft = int(preferred)
	a.ValidLft = int(valid)
	if options.NoDAD {
		a.Flags |= unix.IFA_F_NODAD
	}
	return a, nil
}

func (k *linuxKernel) AddAddress(name, address string, options AddressOptions) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	a, err := nativeAddress(address, options)
	if err != nil {
		return err
	}
	// AddrAdd is exclusive. AddrReplace could overwrite an unowned address.
	return netlink.AddrAdd(l, a)
}

func (k *linuxKernel) UpdateAddress(name, address string, options AddressOptions) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	a, err := nativeAddress(address, options)
	if err != nil {
		return err
	}
	// The engine calls replace only after verifying the address journal, link
	// identity and previous lifetime/DAD options. It never adopts a collision.
	return netlink.AddrReplace(l, a)
}
func (k *linuxKernel) DeleteAddress(name, address string, options AddressOptions) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	a, err := nativeAddressIdentity(address, options)
	if err != nil {
		return err
	}
	// Linux can implicitly remove secondary IPv4 addresses when their primary
	// is removed. Never let deleting one owned address delete someone else's.
	if a.IP.To4() != nil {
		all, err := netlink.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		primary := false
		for _, v := range all {
			if v.IPNet != nil && v.IPNet.String() == address {
				primary = v.Flags&unix.IFA_F_SECONDARY == 0
			}
		}
		if primary {
			target, _ := netip.ParsePrefix(address)
			if options.Peer != "" {
				target, _ = netip.ParsePrefix(options.Peer)
			}
			for _, v := range all {
				if v.IPNet == nil || v.IPNet.String() == address {
					continue
				}
				peer := v.IPNet.String()
				if v.Peer != nil {
					peer = v.Peer.String()
				}
				other, err := netip.ParsePrefix(peer)
				if err == nil && target.Masked() == other.Masked() {
					return fmt.Errorf("removing primary %s could remove other address %s on %s", address, v.IPNet, name)
				}
			}
		}
	}
	return netlink.AddrDel(l, a)
}

func (k *linuxKernel) Routes(table int) ([]RouteState, error) {
	rs, err := nativeRouteDump()
	if err != nil {
		return nil, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	out := make([]RouteState, 0, len(rs))
	observedAt, err := readRouteClock()
	if err != nil {
		return nil, err
	}
	for _, raw := range rs {
		r, err := decodeNativeRoute(raw, names, observedAt)
		if err != nil {
			return nil, err
		}
		if r.Destination == "" || table != 0 && r.Table != table {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if routeSlot(a.Route) == routeSlot(b.Route) {
			return a.Interface+a.Gateway < b.Interface+b.Gateway
		}
		return routeSlot(a.Route) < routeSlot(b.Route)
	})
	return out, nil
}

func nativeRouteState(r netlink.Route, names map[int]string) RouteState {
	dst := "0.0.0.0/0"
	if r.Family == netlink.FAMILY_V6 {
		dst = "::/0"
	}
	if r.Dst != nil {
		dst = r.Dst.String()
	}
	g, src := "", ""
	if len(r.Gw) > 0 {
		g = r.Gw.String()
	}
	if len(r.Src) > 0 {
		src = r.Src.String()
	}
	// DEAD/LINKDOWN reflect current link health, not an externally supplied
	// forwarding policy. ONLINK and all other unmodeled flags remain foreign.
	const healthFlags = unix.RTNH_F_DEAD | unix.RTNH_F_LINKDOWN
	advanced := r.ILinkIndex != 0 || r.Tos != 0 || r.Flags & ^healthFlags != 0 || r.MPLSDst != nil || r.NewDst != nil || r.Encap != nil || r.Via != nil || r.Realm != 0 || r.MTU != 0 || r.MTULock || r.Window != 0 || r.Rtt != 0 || r.RttVar != 0 || r.Ssthresh != 0 || r.Cwnd != 0 || r.AdvMSS != 0 || r.Reordering != 0 || r.Hoplimit != 0 || r.InitCwnd != 0 || r.Features != 0 || r.RtoMin != 0 || r.RtoMinLock || r.InitRwnd != 0 || r.QuickACK != 0 || r.Congctl != "" || r.FastOpenNoCookie != 0
	definition := Route{Destination: dst, Gateway: g, Interface: names[r.LinkIndex], Metric: r.Priority, Table: r.Table}
	for _, h := range r.MultiPath {
		if h == nil {
			advanced = true
			continue
		}
		next := NextHop{Interface: names[h.LinkIndex], Weight: h.Hops + 1}
		if len(h.Gw) > 0 {
			next.Gateway = h.Gw.String()
		}
		if next.Interface == "" || h.Hops < 0 || h.Hops > 255 || h.Flags & ^healthFlags != 0 || h.NewDst != nil || h.Encap != nil || h.Via != nil {
			advanced = true
		}
		definition.NextHops = append(definition.NextHops, next)
	}
	if len(r.MultiPath) > 0 && (g != "" || r.LinkIndex != 0) {
		advanced = true
	}
	definition = normalizeRoute(definition)
	return RouteState{Route: definition, Protocol: int(r.Protocol), Type: r.Type, Source: src, Multipath: len(definition.NextHops) > 0, Advanced: advanced}
}

func linuxRoute(r Route) (*netlink.Route, error) {
	r = normalizeRoute(r)
	_, dst, err := net.ParseCIDR(r.Destination)
	if err != nil {
		return nil, err
	}
	result := &netlink.Route{Dst: dst, Table: routeTable(r), Priority: r.Metric, Protocol: netlink.RouteProtocol(RouteProtocol), Type: unix.RTN_UNICAST}
	if dst.IP.To4() != nil {
		result.Family = netlink.FAMILY_V4
	} else {
		result.Family = netlink.FAMILY_V6
	}
	result.Scope = netlink.SCOPE_LINK
	if len(r.NextHops) > 0 {
		for _, h := range r.NextHops {
			l, err := linuxLink(h.Interface)
			if err != nil {
				return nil, err
			}
			next := &netlink.NexthopInfo{LinkIndex: l.Attrs().Index, Hops: h.Weight - 1}
			if h.Gateway != "" {
				next.Gw = net.ParseIP(h.Gateway)
				result.Scope = netlink.SCOPE_UNIVERSE
			}
			result.MultiPath = append(result.MultiPath, next)
		}
	} else {
		l, err := linuxLink(r.Interface)
		if err != nil {
			return nil, err
		}
		result.LinkIndex = l.Attrs().Index
		if r.Gateway != "" {
			result.Gw = net.ParseIP(r.Gateway)
			result.Scope = netlink.SCOPE_UNIVERSE
		}
	}
	return result, nil
}
func (k *linuxKernel) AddRoute(saved ownedRoute) error {
	return k.writeRoute(saved, false)
}
func (k *linuxKernel) UpdateRoute(saved ownedRoute) error {
	return k.writeRoute(saved, true)
}
func (k *linuxKernel) writeRoute(saved ownedRoute, replace bool) error {
	r := saved.Route
	v, err := linuxRoute(r)
	if err != nil {
		return err
	}
	if saved.KernelExpiry != (routeTimer{}) {
		clock, err := readRouteClock()
		if err != nil {
			return err
		}
		if clock.BootID != saved.KernelExpiry.BootID {
			return errors.New("route timer belongs to a different kernel boot")
		}
		req, err := expiringRouteRequest(v, time.Duration(int64(saved.KernelExpiry.Nanos)-clock.monotonicNanos))
		if err != nil {
			return err
		}
		if replace {
			req.Flags = unix.NLM_F_REQUEST | unix.NLM_F_REPLACE | unix.NLM_F_ACK
		}
		_, err = req.Execute(unix.NETLINK_ROUTE, 0)
		return err
	}
	if replace {
		return netlink.RouteChange(v)
	}
	return netlink.RouteAdd(v)
}
func (k *linuxKernel) DeleteRoute(r Route) error {
	v, err := linuxRoute(r)
	if err != nil {
		return err
	}
	return netlink.RouteDel(v)
}

func settingPath(key settingKey) (string, error) {
	switch key.Kind {
	case "conntrack_tcp_liberal":
		return "/proc/sys/net/netfilter/nf_conntrack_tcp_be_liberal", nil
	case "multipath_hash_fields4":
		return "/proc/sys/net/ipv4/fib_multipath_hash_fields", nil
	case "multipath_hash_policy4":
		return "/proc/sys/net/ipv4/fib_multipath_hash_policy", nil
	case "forwarding4":
		return "/proc/sys/net/ipv4/ip_forward", nil
	case "forwarding6":
		return "/proc/sys/net/ipv6/conf/all/forwarding", nil
	case "rp_filter_all":
		return "/proc/sys/net/ipv4/conf/all/rp_filter", nil
	case "rp_filter_default":
		return "/proc/sys/net/ipv4/conf/default/rp_filter", nil
	case "arp_ignore", "arp_announce":
		if !validName(key.Interface) || key.Interface == "all" || key.Interface == "default" {
			return "", fmt.Errorf("invalid per-interface IPv4 sysctl identity %q", key.Interface)
		}
		return "/proc/sys/net/ipv4/conf/" + key.Interface + "/" + key.Kind, nil
	case "accept_ra", "autoconf", "ipv6_mtu", "hop_limit":
		if !validName(key.Interface) || key.Interface == "all" || key.Interface == "default" {
			return "", fmt.Errorf("invalid per-interface IPv6 sysctl identity %q", key.Interface)
		}
		name := key.Kind
		if name == "ipv6_mtu" {
			name = "mtu"
		}
		return "/proc/sys/net/ipv6/conf/" + key.Interface + "/" + name, nil
	}
	return "", fmt.Errorf("unknown sysctl setting %q", key.Kind)
}
func (k *linuxKernel) ReadSetting(key settingKey) (int, error) {
	if key.Kind == "mtu" || key.Kind == "up" {
		l, err := linuxLink(key.Interface)
		if err != nil {
			return 0, err
		}
		if key.Kind == "mtu" {
			return l.Attrs().MTU, nil
		}
		return boolInt(l.Attrs().Flags&net.FlagUp != 0), nil
	}
	path, err := settingPath(key)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
func (k *linuxKernel) WriteSetting(key settingKey, value int) error {
	if key.Kind == "mtu" || key.Kind == "up" {
		l, err := linuxLink(key.Interface)
		if err != nil {
			return err
		}
		if key.Kind == "mtu" {
			return netlink.LinkSetMTU(l, value)
		}
		if value == 1 {
			return netlink.LinkSetUp(l)
		}
		return netlink.LinkSetDown(l)
	}
	path, err := settingPath(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(value)+"\n"), 0600)
}
