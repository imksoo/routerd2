//go:build linux

package dhcprelay

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/ethernet"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

type clientLink struct {
	name    string
	address netip.Addr
	servers []netip.Addr
	index   int
	mac     net.HardwareAddr
	mtu     int
}

func (l clientLink) check(links *netlink.Handle, vrf string) error {
	link, err := links.LinkByIndex(l.index)
	if err != nil {
		return err
	}
	a := link.Attrs()
	if a.Name != l.name || a.RawFlags&unix.IFF_UP == 0 || a.MTU != l.mtu || string(a.HardwareAddr) != string(l.mac) {
		return errors.New("DHCP relay interface identity or readiness changed")
	}
	actualVRF := ""
	master := a.MasterIndex
	for depth := 0; master != 0 && depth < 16; depth++ {
		parent, err := links.LinkByIndex(master)
		if err != nil {
			return err
		}
		if parent.Type() == "vrf" {
			actualVRF, master = parent.Attrs().Name, 0
			break
		}
		master = parent.Attrs().MasterIndex
	}
	if master != 0 || actualVRF != vrf {
		return errors.New("DHCP relay interface VRF changed")
	}
	addresses, err := links.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if address.IP.Equal(net.IP(l.address.AsSlice())) && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			return nil
		}
	}
	return errors.New("DHCP relay giaddr is not installed on its client interface")
}

// Run owns one UDP listener in the routing domain and one output-only Ethernet
// socket. The server owns leases and retries; the relay keeps no transaction map.
func Run(ctx context.Context, c dhcprelayconfig.Config, ready func()) error {
	if err := dhcprelayconfig.Validate(c); err != nil {
		return err
	}
	if c.MaxHops == 0 {
		c.MaxHops = 4
	}
	links, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer links.Close()
	if err := links.SetSocketTimeout(250 * time.Millisecond); err != nil {
		return err
	}
	// Namespace-scoped claims also exclude native servers on client interfaces.
	var claims []int
	defer func() {
		for _, fd := range claims {
			_ = unix.Close(fd)
		}
	}()
	claim := func(name string) error {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
		if err := unix.Bind(fd, &unix.SockaddrUnix{Name: "\x00" + name}); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("claim DHCP relay listener: %w", err)
		}
		claims = append(claims, fd)
		return nil
	}
	if err := claim("routerd2-dhcp-relay:" + c.VRF); err != nil {
		return err
	}
	byIndex := make(map[int]clientLink, len(c.Interfaces))
	byAddress := make(map[netip.Addr]clientLink, len(c.Interfaces))
	for _, iface := range c.Interfaces {
		link, err := links.LinkByName(iface.Name)
		if err != nil {
			return err
		}
		a := link.Attrs()
		if len(a.HardwareAddr) != 6 || a.MTU < 576 {
			return errors.New("DHCP relay requires an Ethernet interface with MTU >= 576")
		}
		client := clientLink{name: iface.Name, address: netip.MustParseAddr(iface.Address), index: a.Index, mac: append(net.HardwareAddr(nil), a.HardwareAddr...), mtu: a.MTU}
		for _, server := range iface.Servers {
			client.servers = append(client.servers, netip.MustParseAddr(server))
		}
		if err := client.check(links, c.VRF); err != nil {
			return err
		}
		if err := claim(fmt.Sprintf("routerd2-dhcp-listener:%d", a.Index)); err != nil {
			return err
		}
		byIndex[a.Index] = client
		byAddress[client.address] = client
	}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if inner == nil && c.VRF != "" {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.VRF)
			}
		})
		return errors.Join(err, inner)
	}}
	udp, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:67")
	if err != nil {
		return err
	}
	defer udp.Close()
	conn := ipv4.NewPacketConn(udp)
	if err := conn.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
		return err
	}
	tx, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer unix.Close(tx)
	var idSeed [2]byte
	if _, err := rand.Read(idSeed[:]); err != nil {
		return err
	}
	ipID := binary.BigEndian.Uint16(idSeed[:])
	ready()
	buf := make([]byte, 4097)
	nextCheck := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(nextCheck) {
			for _, client := range byIndex {
				if err := client.check(links, c.VRF); err != nil {
					return err
				}
			}
			nextCheck = time.Now().Add(time.Second)
		}
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return err
		}
		n, metadata, source, err := conn.ReadFrom(buf)
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				continue
			}
			return err
		}
		if n < 236 || n > 4096 || metadata == nil || buf[1] != 1 || buf[2] != 6 {
			continue
		}
		packet := buf[:n]
		from := source.(*net.UDPAddr)
		giaddr := netip.AddrFrom4([4]byte(packet[24:28]))
		switch packet[0] {
		case 1: // BOOTREQUEST: only hops and an initially empty giaddr change.
			client, local := byIndex[metadata.IfIndex]
			if !local || from.Port != 68 && from.Port != 67 || int(packet[3]) > c.MaxHops {
				continue
			}
			if _, loop := byAddress[giaddr]; loop {
				continue
			}
			if err := client.check(links, c.VRF); err != nil {
				return err
			}
			if giaddr.IsUnspecified() {
				address := client.address.As4()
				copy(packet[24:28], address[:])
			}
			packet[3]++
			deadline := time.Now().Add(time.Second)
			if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
				deadline = until
			}
			if err := conn.SetWriteDeadline(deadline); err != nil {
				return err
			}
			for _, server := range client.servers {
				if _, err := conn.WriteTo(packet, &ipv4.ControlMessage{Src: net.IP(client.address.AsSlice())}, &net.UDPAddr{IP: net.IP(server.AsSlice()), Port: 67}); err != nil {
					return fmt.Errorf("forward DHCP request: %w", err)
				}
			}
		case 2: // BOOTREPLY: preserve the complete payload, including options.
			client, local := byAddress[giaddr]
			fromAddress, ok := netip.AddrFromSlice(from.IP)
			if !local || !ok || from.Port != 67 || !slices.Contains(client.servers, fromAddress.Unmap()) {
				continue
			}
			if err := client.check(links, c.VRF); err != nil {
				return err
			}
			destination := netip.AddrFrom4([4]byte(packet[16:20]))
			if destination.IsUnspecified() {
				destination = netip.AddrFrom4([4]byte(packet[12:16])) // DHCPINFORM ACK
			}
			mac := net.HardwareAddr(packet[28:34])
			if binary.BigEndian.Uint16(packet[10:12])&0x8000 != 0 || destination.IsUnspecified() {
				destination = netip.AddrFrom4([4]byte{255, 255, 255, 255})
				mac = net.HardwareAddr{255, 255, 255, 255, 255, 255}
			}
			ipID++
			frames, err := ethernet.UDP4(client.mac, mac, fromAddress.Unmap(), destination, 67, 68, ipID, client.mtu, packet)
			if err != nil {
				return err
			}
			for _, frame := range frames {
				if err := unix.Sendto(tx, frame, 0, &unix.SockaddrLinklayer{Ifindex: client.index}); err != nil && err != unix.EAGAIN && err != unix.ENOBUFS && err != unix.EINTR {
					return fmt.Errorf("forward DHCP reply: %w", err)
				}
			}
		}
	}
}
