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
	a := l.Attrs()
	out := InterfaceState{Name: a.Name, Index: a.Index, Kind: l.Type(), MTU: a.MTU, Up: a.Flags&net.FlagUp != 0, Alias: a.Alias}
	as, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return out, err
	}
	for _, addr := range as {
		if addr.IPNet != nil {
			out.Addresses = append(out.Addresses, AddressState{Address: addr.IPNet.String(), Flags: addr.Flags})
		}
	}
	sort.Slice(out.Addresses, func(i, j int) bool { return out.Addresses[i].Address < out.Addresses[j].Address })
	if t, ok := l.(*netlink.Ip6tnl); ok && t.Proto == unix.IPPROTO_IPIP {
		parent, err := netlink.LinkByIndex(int(t.Link))
		if err != nil {
			return out, fmt.Errorf("tunnel underlay: %w", err)
		}
		out.DSLite = &DSLite{Name: a.Name, Underlay: parent.Attrs().Name, Local: t.Local.String(), Remote: t.Remote.String(), MTU: a.MTU}
	}
	return out, nil
}

func (k *linuxKernel) AddAddress(name, address string) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	a, err := netlink.ParseAddr(address)
	if err != nil {
		return err
	}
	// AddrAdd is exclusive. AddrReplace could overwrite an unowned address.
	return netlink.AddrAdd(l, a)
}
func (k *linuxKernel) DeleteAddress(name, address string) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	a, err := netlink.ParseAddr(address)
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
			for _, v := range all {
				if v.IPNet == nil || v.IPNet.String() == address {
					continue
				}
				other, err := netip.ParsePrefix(v.IPNet.String())
				if err == nil && target.Masked() == other.Masked() {
					return fmt.Errorf("removing primary %s could remove other address %s on %s", address, v.IPNet, name)
				}
			}
		}
	}
	return netlink.AddrDel(l, a)
}

func (k *linuxKernel) Routes(table int) ([]RouteState, error) {
	rs, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
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
	for _, r := range rs {
		if r.Family != netlink.FAMILY_V4 && r.Family != netlink.FAMILY_V6 {
			continue
		}
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
		advanced := r.ILinkIndex != 0 || r.Tos != 0 || r.Flags != 0 || r.MPLSDst != nil || r.NewDst != nil || r.Encap != nil || r.Via != nil || r.Realm != 0 || r.MTU != 0 || r.MTULock || r.Window != 0 || r.Rtt != 0 || r.RttVar != 0 || r.Ssthresh != 0 || r.Cwnd != 0 || r.AdvMSS != 0 || r.Reordering != 0 || r.Hoplimit != 0 || r.InitCwnd != 0 || r.Features != 0 || r.RtoMin != 0 || r.RtoMinLock || r.InitRwnd != 0 || r.QuickACK != 0 || r.Congctl != "" || r.FastOpenNoCookie != 0
		out = append(out, RouteState{Route: Route{Destination: dst, Gateway: g, Interface: names[r.LinkIndex], Metric: r.Priority, Table: r.Table}, Protocol: int(r.Protocol), Type: r.Type, Source: src, Multipath: len(r.MultiPath) > 0, Advanced: advanced})
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

func linuxRoute(r Route) (*netlink.Route, error) {
	l, err := linuxLink(r.Interface)
	if err != nil {
		return nil, err
	}
	_, dst, err := net.ParseCIDR(r.Destination)
	if err != nil {
		return nil, err
	}
	result := &netlink.Route{LinkIndex: l.Attrs().Index, Dst: dst, Table: routeTable(r), Priority: r.Metric, Protocol: netlink.RouteProtocol(RouteProtocol), Type: unix.RTN_UNICAST}
	if dst.IP.To4() != nil {
		result.Family = netlink.FAMILY_V4
	} else {
		result.Family = netlink.FAMILY_V6
	}
	if r.Gateway != "" {
		result.Gw = net.ParseIP(r.Gateway)
	} else {
		result.Scope = netlink.SCOPE_LINK
	}
	return result, nil
}
func (k *linuxKernel) AddRoute(r Route) error {
	v, err := linuxRoute(r)
	if err != nil {
		return err
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

func linuxTunnel(t DSLite) (*netlink.Ip6tnl, error) {
	parent, err := linuxLink(t.Underlay)
	if err != nil {
		return nil, err
	}
	return &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: t.Name, Alias: tunnelAlias, MTU: t.MTU}, Link: uint32(parent.Attrs().Index), Local: net.ParseIP(t.Local), Remote: net.ParseIP(t.Remote), Proto: unix.IPPROTO_IPIP, Ttl: 64, Flags: 1, EncapLimit: 0}, nil
}
func (k *linuxKernel) CreateDSLite(t DSLite) error {
	v, err := linuxTunnel(t)
	if err != nil {
		return err
	}
	return netlink.LinkAdd(v)
}
func (k *linuxKernel) UpdateDSLite(t DSLite) error {
	l, err := linuxLink(t.Name)
	if err != nil {
		return err
	}
	v, err := linuxTunnel(t)
	if err != nil {
		return err
	}
	v.Index = l.Attrs().Index
	return netlink.LinkModify(v)
}
func (k *linuxKernel) DeleteDSLite(name string) error {
	l, err := linuxLink(name)
	if err != nil {
		return err
	}
	return netlink.LinkDel(l)
}

func settingPath(key settingKey) (string, error) {
	switch key.Kind {
	case "forwarding4":
		return "/proc/sys/net/ipv4/ip_forward", nil
	case "forwarding6":
		return "/proc/sys/net/ipv6/conf/all/forwarding", nil
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
