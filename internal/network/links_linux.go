//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func parentIndex(l netlink.Link) int {
	switch v := l.(type) {
	case *netlink.Vlan, *netlink.Macvlan:
		return l.Attrs().ParentIndex
	case *netlink.Vxlan:
		return v.VtepDevIndex
	case *netlink.Ip6tnl:
		if v.Link != 0 {
			return int(v.Link)
		}
		return v.Attrs().ParentIndex
	}
	return 0
}

func nativeLinkState(l netlink.Link) (InterfaceState, error) {
	a := l.Attrs()
	out := InterfaceState{Name: a.Name, Index: a.Index, Kind: l.Type(), MTU: a.MTU, Up: a.Flags&net.FlagUp != 0, Alias: a.Alias, HardwareAddr: a.HardwareAddr.String(), MasterIndex: a.MasterIndex}
	if index := parentIndex(l); index > 0 {
		parent, err := netlink.LinkByIndex(index)
		if err != nil {
			return out, fmt.Errorf("%s parent: %w", a.Name, err)
		}
		out.Parent = parent.Attrs().Name
	}
	if a.MasterIndex > 0 {
		master, err := netlink.LinkByIndex(a.MasterIndex)
		if err != nil {
			return out, fmt.Errorf("%s master: %w", a.Name, err)
		}
		out.Master = master.Attrs().Name
	}
	definition := Link{Name: a.Name, Kind: l.Type(), Parent: out.Parent, Alias: userLinkAlias(a.Alias), HardwareAddr: a.HardwareAddr.String()}
	switch v := l.(type) {
	case *netlink.Dummy:
	case *netlink.Wireguard:
		definition.HardwareAddr = ""
	case *netlink.Bridge:
		definition.BridgeVLANFiltering = v.VlanFiltering
		definition.BridgeMulticastSnooping = v.MulticastSnooping
	case *netlink.Vlan:
		// This API deliberately represents only 802.1Q; a foreign 802.1ad link
		// cannot pass the ownership/definition readback check.
		if v.VlanProtocol != netlink.VLAN_PROTOCOL_8021Q {
			return out, nil
		}
		definition.VLANID = v.VlanId
	case *netlink.Vrf:
		definition.VRFTable = int(v.Table)
		definition.HardwareAddr = ""
	case *netlink.Vxlan:
		definition.VXLANID = v.VxlanId
		if len(v.SrcAddr) > 0 && !v.SrcAddr.IsUnspecified() {
			definition.Local = v.SrcAddr.String()
		}
		if len(v.Group) > 0 && !v.Group.IsUnspecified() {
			if v.Group.IsMulticast() {
				definition.Group = v.Group.String()
			} else {
				definition.Remote = v.Group.String()
			}
		}
		definition.Port = v.Port
		definition.TTL = v.TTL
		definition.Learning = boolPointer(v.Learning)
	case *netlink.Macvlan:
		switch v.Mode {
		case netlink.MACVLAN_MODE_BRIDGE:
			definition.MACVLANMode = "bridge"
		case netlink.MACVLAN_MODE_PRIVATE:
			definition.MACVLANMode = "private"
		default:
			return out, nil
		}
	default:
		return out, nil
	}
	if mode, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + a.Name + "/addr_gen_mode"); err == nil {
		switch strings.TrimSpace(string(mode)) {
		case "0":
			definition.IPv6AddrGenMode = "eui64"
		case "1":
			definition.IPv6AddrGenMode = "none"
		default:
			definition.IPv6AddrGenMode = "kernel:" + strings.TrimSpace(string(mode))
		}
	} else if !os.IsNotExist(err) {
		return out, fmt.Errorf("%s IPv6 address generation mode: %w", a.Name, err)
	}
	out.Link = &definition
	return out, nil
}

func (k *linuxKernel) Interfaces() ([]InterfaceState, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	var out []InterfaceState
	for _, l := range links {
		state, err := nativeLinkState(l)
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nativeLink(l Link, alias string) (netlink.Link, error) {
	a := netlink.NewLinkAttrs()
	a.Name = l.Name
	a.Alias = alias
	if l.HardwareAddr != "" {
		mac, err := net.ParseMAC(l.HardwareAddr)
		if err != nil {
			return nil, err
		}
		a.HardwareAddr = mac
	}
	if l.Parent != "" {
		parent, err := linuxLink(l.Parent)
		if err != nil {
			return nil, err
		}
		a.ParentIndex = parent.Attrs().Index
	}
	switch l.Kind {
	case "dummy":
		return &netlink.Dummy{LinkAttrs: a}, nil
	case "wireguard":
		return &netlink.Wireguard{LinkAttrs: a}, nil
	case "bridge":
		return &netlink.Bridge{LinkAttrs: a, VlanFiltering: l.BridgeVLANFiltering, MulticastSnooping: l.BridgeMulticastSnooping}, nil
	case "vlan":
		return &netlink.Vlan{LinkAttrs: a, VlanId: l.VLANID, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}, nil
	case "vrf":
		return &netlink.Vrf{LinkAttrs: a, Table: uint32(l.VRFTable)}, nil
	case "macvlan":
		mode := netlink.MACVLAN_MODE_BRIDGE
		if l.MACVLANMode == "private" {
			mode = netlink.MACVLAN_MODE_PRIVATE
		}
		return &netlink.Macvlan{LinkAttrs: a, Mode: mode}, nil
	case "vxlan":
		group := l.Remote
		if l.Group != "" {
			group = l.Group
		}
		learning := true
		if l.Learning != nil {
			learning = *l.Learning
		}
		return &netlink.Vxlan{LinkAttrs: a, VxlanId: l.VXLANID, VtepDevIndex: a.ParentIndex, SrcAddr: net.ParseIP(l.Local), Group: net.ParseIP(group), Port: l.Port, TTL: l.TTL, Learning: learning}, nil
	default:
		return nil, fmt.Errorf("unsupported link kind %q", l.Kind)
	}
}

func (k *linuxKernel) CreateLink(l Link, alias string) error {
	native, err := nativeLink(l, alias)
	if err != nil {
		return fmt.Errorf("prepare %s link %s: %w", l.Kind, l.Name, err)
	}
	if err := netlink.LinkAdd(native); err != nil {
		return fmt.Errorf("create %s link %s: %w", l.Kind, l.Name, err)
	}
	if l.IPv6AddrGenMode != "" {
		mode := 0
		if l.IPv6AddrGenMode == "none" {
			mode = 1
		}
		if err := netlink.LinkSetIP6AddrGenMode(native, mode); err != nil {
			return errors.Join(fmt.Errorf("%s link %s IPv6 address generation: %w", l.Kind, l.Name, err), netlink.LinkDel(native))
		}
	}
	// LinkAdd does not consistently serialize IFLA_IFALIAS across link types.
	// Set and verify it before recording ownership. A failed marker write rolls
	// back only the just-created, exclusively named device.
	if err := netlink.LinkSetAlias(native, alias); err != nil {
		return errors.Join(fmt.Errorf("%s link %s ownership alias: %w", l.Kind, l.Name, err), netlink.LinkDel(native))
	}
	return nil
}

func (k *linuxKernel) UpdateLink(l Link, alias string) (result error) {
	live, err := linuxLink(l.Name)
	if err != nil {
		return err
	}
	mutationAttempted := false
	defer func() {
		if result != nil && mutationAttempted {
			result = &partialMutationError{result}
		}
	}()
	if l.IPv6AddrGenMode != "" {
		mode := 0
		if l.IPv6AddrGenMode == "none" {
			mode = 1
		}
		current, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + l.Name + "/addr_gen_mode")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(current)) != strconv.Itoa(mode) {
			mutationAttempted = true
			if err := netlink.LinkSetIP6AddrGenMode(live, mode); err != nil {
				return fmt.Errorf("link %s IPv6 address generation: %w", l.Name, err)
			}
		}
	}
	switch l.Kind {
	case "bridge":
		// Do not pass the observed Bridge back to LinkModify: that would replay
		// every observed attribute (including flags and kernel-derived timers).
		// Send only the fields this typed API actually owns.
		minimal := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: l.Name, Index: live.Attrs().Index}, VlanFiltering: l.BridgeVLANFiltering, MulticastSnooping: l.BridgeMulticastSnooping}
		mutationAttempted = true
		if err := netlink.LinkModify(minimal); err != nil {
			return fmt.Errorf("bridge %s options: %w", l.Name, err)
		}
	case "vxlan":
		mutationAttempted = true
		if err := updateNativeVXLAN(live, l); err != nil {
			return fmt.Errorf("VXLAN %s options: %w", l.Name, err)
		}
	}
	if l.HardwareAddr != "" && l.HardwareAddr != live.Attrs().HardwareAddr.String() {
		mac, err := net.ParseMAC(l.HardwareAddr)
		if err != nil {
			return err
		}
		mutationAttempted = true
		if err := netlink.LinkSetHardwareAddr(live, mac); err != nil {
			return fmt.Errorf("link %s MAC: %w", l.Name, err)
		}
	}
	if live.Attrs().Alias != alias {
		mutationAttempted = true
		if err := netlink.LinkSetAlias(live, alias); err != nil {
			return fmt.Errorf("link %s alias: %w", l.Name, err)
		}
	}
	return nil
}

func updateNativeVXLAN(live netlink.Link, l Link) error {
	// Linux rejects immutable attributes even when their values are unchanged.
	// netlink.LinkModify(Vxlan) includes port, checksum and proxy flags; use a
	// narrowly scoped RTM_NEWLINK request for the mutable options instead.
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	message := nl.NewIfInfomsg(unix.AF_UNSPEC)
	message.Index = int32(live.Attrs().Index)
	req.AddData(message)
	info := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	info.AddRtAttr(nl.IFLA_INFO_KIND, nl.ZeroTerminated("vxlan"))
	data := info.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	group := l.Remote
	if l.Group != "" {
		group = l.Group
	}
	for _, endpoint := range []struct {
		value  string
		v4, v6 int
	}{{l.Local, nl.IFLA_VXLAN_LOCAL, nl.IFLA_VXLAN_LOCAL6}, {group, nl.IFLA_VXLAN_GROUP, nl.IFLA_VXLAN_GROUP6}} {
		ip := net.ParseIP(endpoint.value)
		if ip == nil {
			if vxlanFamily(l) == 6 {
				ip = net.IPv6zero
			} else {
				ip = net.IPv4zero
			}
		}
		if v4 := ip.To4(); v4 != nil {
			data.AddRtAttr(endpoint.v4, v4)
		} else {
			data.AddRtAttr(endpoint.v6, ip.To16())
		}
	}
	data.AddRtAttr(nl.IFLA_VXLAN_TTL, nl.Uint8Attr(uint8(l.TTL)))
	learning := true
	if l.Learning != nil {
		learning = *l.Learning
	}
	data.AddRtAttr(nl.IFLA_VXLAN_LEARNING, nl.Uint8Attr(uint8(boolInt(learning))))
	req.AddData(info)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func (k *linuxKernel) DeleteLink(name string) error {
	live, err := linuxLink(name)
	if err != nil {
		return err
	}
	return netlink.LinkDel(live)
}

func (k *linuxKernel) WriteAttribute(key settingKey, value string) error {
	live, err := linuxLink(key.Interface)
	if err != nil {
		return err
	}
	switch key.Kind {
	case "alias":
		return netlink.LinkSetAlias(live, value)
	case "master":
		if value == "" {
			return netlink.LinkSetNoMaster(live)
		}
		master, err := linuxLink(value)
		if err != nil {
			return err
		}
		if master.Type() != "bridge" && master.Type() != "vrf" {
			return fmt.Errorf("%s is not a bridge or VRF", value)
		}
		return netlink.LinkSetMaster(live, master)
	default:
		return fmt.Errorf("unknown link attribute %q", key.Kind)
	}
}
