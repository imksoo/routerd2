//go:build linux

package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Quarantine has its own netlink sockets. Closing them at the cleanup deadline
// interrupts pending Go netpoll I/O without leaving a mutating goroutine behind
// or changing global socket timeouts used by other workers.
func (k *linuxKernel) QuarantineWireGuard(ctx context.Context, name string, index int, ownsLink func(InterfaceState) bool, ownsDevice func(wireGuardDevice) bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	route, err := nl.Subscribe(unix.NETLINK_ROUTE)
	if err != nil {
		return false, err
	}
	defer route.Close()
	generic, err := nl.Subscribe(unix.NETLINK_GENERIC)
	if err != nil {
		return false, err
	}
	defer generic.Close()
	stop := context.AfterFunc(ctx, func() {
		route.Close()
		generic.Close()
	})
	defer stop()
	sockets := map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE:   {Socket: route},
		unix.NETLINK_GENERIC: {Socket: generic},
	}
	link, err := readWireGuardQuarantineLink(sockets, index)
	if err != nil {
		return false, errors.Join(err, ctx.Err())
	}
	if link.Name != name || !ownsLink(link) {
		return false, errors.New("WireGuard quarantine refused: link ownership changed")
	}
	family, err := wireGuardQuarantineFamily(sockets)
	if err != nil {
		return false, errors.Join(err, ctx.Err())
	}
	req := wireGuardRequest(family, unix.WG_CMD_GET_DEVICE, index, unix.NLM_F_DUMP)
	req.Sockets = sockets
	device, err := readWireGuardRequest(index, req, family)
	owned := err == nil && ownsDevice(device)
	clearWireGuardDevice(&device)
	if err != nil {
		return false, errors.Join(err, ctx.Err())
	}
	if !owned {
		return false, errors.New("WireGuard quarantine refused: device configuration changed outside routerd2")
	}
	if !link.Up {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	req = nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	req.Sockets = sockets
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index, msg.Change = int32(index), unix.IFF_UP
	req.AddData(msg)
	_, writeErr := req.Execute(unix.NETLINK_ROUTE, 0)
	link, err = readWireGuardQuarantineLink(sockets, index)
	if err != nil || link.Name != name || !ownsLink(link) || link.Up {
		return true, errors.Join(writeErr, err, ctx.Err(), errors.New("WireGuard quarantine could not verify the owned device DOWN"))
	}
	// An acknowledged readback also resolves a lost DOWN acknowledgment.
	return true, nil
}

func readWireGuardQuarantineLink(sockets map[int]*nl.SocketHandle, index int) (InterfaceState, error) {
	var link InterfaceState
	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, 0)
	req.Sockets = sockets
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(index)
	req.AddData(msg)
	messages, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil {
		return link, err
	}
	if len(messages) != 1 || len(messages[0]) < unix.SizeofIfInfomsg {
		return link, errors.New("invalid WireGuard quarantine link response")
	}
	info := nl.DeserializeIfInfomsg(messages[0])
	if int(info.Index) != index {
		return link, errors.New("WireGuard quarantine link index differs")
	}
	link.Index, link.Up = index, info.Flags&unix.IFF_UP != 0
	attrs, err := wireGuardAttributes(messages[0][unix.SizeofIfInfomsg:])
	if err != nil {
		return link, err
	}
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case unix.IFLA_IFNAME:
			link.Name = nl.BytesToString(attr.Value)
		case unix.IFLA_IFALIAS:
			link.Alias = nl.BytesToString(attr.Value)
		case unix.IFLA_LINKINFO:
			nested, err := wireGuardAttributes(attr.Value)
			if err != nil {
				return link, err
			}
			for _, value := range nested {
				if value.Attr.Type == unix.IFLA_INFO_KIND {
					link.Kind = nl.BytesToString(value.Value)
				}
			}
		}
	}
	if !validName(link.Name) || link.Kind != "wireguard" {
		return link, errors.New("quarantine target is not a WireGuard interface")
	}
	return link, nil
}

func wireGuardQuarantineFamily(sockets map[int]*nl.SocketHandle) (uint16, error) {
	req := nl.NewNetlinkRequest(nl.GENL_ID_CTRL, 0)
	req.Sockets = sockets
	req.AddData(&nl.Genlmsg{Command: nl.GENL_CTRL_CMD_GETFAMILY, Version: nl.GENL_CTRL_VERSION})
	req.AddData(wireGuardAttr(nl.GENL_CTRL_ATTR_FAMILY_NAME, nl.ZeroTerminated("wireguard")))
	messages, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return 0, err
	}
	if len(messages) != 1 || len(messages[0]) < nl.SizeofGenlmsg {
		return 0, errors.New("invalid WireGuard quarantine family response")
	}
	attrs, err := wireGuardAttributes(messages[0][nl.SizeofGenlmsg:])
	if err != nil {
		return 0, err
	}
	var id uint16
	var version uint32
	var name string
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case nl.GENL_CTRL_ATTR_FAMILY_ID:
			if len(attr.Value) != 2 {
				return 0, errors.New("invalid WireGuard quarantine family ID")
			}
			id = binary.NativeEndian.Uint16(attr.Value)
		case nl.GENL_CTRL_ATTR_VERSION:
			if len(attr.Value) != 4 {
				return 0, errors.New("invalid WireGuard quarantine family version")
			}
			version = binary.NativeEndian.Uint32(attr.Value)
		case nl.GENL_CTRL_ATTR_FAMILY_NAME:
			name = nl.BytesToString(attr.Value)
		}
	}
	if id == 0 || version != wireGuardGenlVersion || name != "wireguard" {
		return 0, fmt.Errorf("unexpected WireGuard quarantine family identity/version %d", version)
	}
	return id, nil
}
