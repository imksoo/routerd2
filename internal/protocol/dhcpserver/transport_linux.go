// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type nativeTransport struct {
	config       Config
	index        int
	mac          net.HardwareAddr
	mtu          int
	source       netip.Addr
	udp          *net.UDPConn
	txFD, lockFD int
	links        *netlink.Handle
	ipID         uint16
}

func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}
func openTransport(c Config) (_ *nativeTransport, result error) {
	t := &nativeTransport{config: c, txFD: -1, lockFD: -1}
	defer func() {
		if result != nil {
			_ = t.Close()
		}
	}()
	var err error
	t.links, err = netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	if err = t.links.SetSocketTimeout(250 * time.Millisecond); err != nil {
		return nil, err
	}
	link, err := t.links.LinkByName(c.Interface)
	if err != nil {
		return nil, err
	}
	t.index = link.Attrs().Index
	t.mac = append(net.HardwareAddr(nil), link.Attrs().HardwareAddr...)
	t.mtu = link.Attrs().MTU
	t.source, _ = netip.ParseAddr(c.ServerAddress)
	if err = t.Check(); err != nil {
		return nil, err
	}
	// An abstract namespace-scoped lock prevents two native instances from
	// consuming the same broadcast stream, including after a PID 1 restart.
	t.lockFD, err = unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err = unix.Bind(t.lockFD, &unix.SockaddrUnix{Name: fmt.Sprintf("\x00routerd2-dhcp-server:%d", t.index)}); err != nil {
		return nil, fmt.Errorf("claim DHCP interface instance: %w", err)
	}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			for _, opt := range []struct{ level, name, value int }{{unix.SOL_SOCKET, unix.SO_REUSEADDR, 1}, {unix.SOL_SOCKET, unix.SO_BROADCAST, 1}, {unix.IPPROTO_IP, unix.IP_PKTINFO, 1}, {unix.SOL_SOCKET, unix.SO_RCVBUF, 256 << 10}} {
				if inner = unix.SetsockoptInt(int(fd), opt.level, opt.name, opt.value); inner != nil {
					return
				}
			}
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface)
		})
		return errors.Join(err, inner)
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:67")
	if err != nil {
		return nil, err
	}
	t.udp = pc.(*net.UDPConn)
	// Protocol 0 makes this an output-only AF_PACKET socket; ordinary routed
	// traffic is not copied into an unused raw receive queue.
	t.txFD, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err = unix.Bind(t.txFD, &unix.SockaddrLinklayer{Ifindex: t.index}); err != nil {
		return nil, err
	}
	return t, nil
}
func (t *nativeTransport) Check() error {
	link, err := t.links.LinkByIndex(t.index)
	if err != nil {
		return err
	}
	a := link.Attrs()
	if a.Name != t.config.Interface || len(a.HardwareAddr) != 6 || string(a.HardwareAddr) != string(t.mac) || a.RawFlags&unix.IFF_UP == 0 || a.MTU < 576 || a.MTU != t.mtu {
		return errors.New("DHCP serving interface identity, MAC, MTU or readiness changed")
	}
	actualVRF := ""
	master := a.MasterIndex
	for depth := 0; master != 0 && depth < 16; depth++ {
		parent, err := t.links.LinkByIndex(master)
		if err != nil {
			return err
		}
		if parent.Type() == "vrf" {
			actualVRF = parent.Attrs().Name
			master = 0
			break
		}
		master = parent.Attrs().MasterIndex
	}
	if master != 0 || actualVRF != t.config.VRF {
		return errors.New("DHCP interface has a different VRF membership")
	}
	addresses, err := t.links.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if ok && ip.Unmap() == t.source && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			return nil
		}
	}
	return errors.New("DHCP server address is not owned on the serving interface")
}
func (t *nativeTransport) Close() error {
	var err error
	if t.udp != nil {
		err = errors.Join(err, t.udp.Close())
		t.udp = nil
	}
	if t.txFD >= 0 {
		err = errors.Join(err, unix.Close(t.txFD))
		t.txFD = -1
	}
	if t.lockFD >= 0 {
		err = errors.Join(err, unix.Close(t.lockFD))
		t.lockFD = -1
	}
	if t.links != nil {
		t.links.Close()
		t.links = nil
	}
	return err
}
func (t *nativeTransport) Receive(ctx context.Context) (packet, error) {
	buf := make([]byte, 4097)
	oob := make([]byte, 128)
	for {
		if err := ctx.Err(); err != nil {
			return packet{}, err
		}
		if err := t.udp.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return packet{}, err
		}
		n, control, flags, from, err := t.udp.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return packet{}, err
		}
		if n > 4096 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || !from.Addr().Is4() {
			continue
		}
		cms, err := unix.ParseSocketControlMessage(oob[:control])
		if err != nil {
			continue
		}
		index := 0
		destination := netip.Addr{}
		for _, cm := range cms {
			if cm.Header.Level == unix.IPPROTO_IP && cm.Header.Type == unix.IP_PKTINFO && len(cm.Data) >= 12 {
				index = int(binary.NativeEndian.Uint32(cm.Data[:4]))
				destination = addr4(cm.Data[8:12])
			}
		}
		if index != t.index || !destination.Is4() {
			continue
		}
		return packet{Data: append([]byte(nil), buf[:n]...), Source: from.Addr(), SourcePort: int(from.Port()), Destination: destination, InterfaceIndex: index}, nil
	}
}
func (t *nativeTransport) Send(ctx context.Context, r response) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.Check(); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if r.Routed {
		if err := t.udp.SetWriteDeadline(deadline); err != nil {
			return err
		}
		control := unix.PktInfo4(&unix.Inet4Pktinfo{Ifindex: int32(t.index), Spec_dst: t.source.As4()})
		n, _, err := t.udp.WriteMsgUDPAddrPort(r.Data, control, netip.AddrPortFrom(r.Destination, uint16(r.Port)))
		if err == nil && n != len(r.Data) {
			err = errors.New("short DHCP UDP write")
		}
		return err
	}
	t.ipID++
	frame, err := udpFrame(t.mac, r.MAC, t.source, r.Destination, 67, uint16(r.Port), t.ipID, r.Data)
	if err != nil {
		return err
	}
	for {
		err := unix.Sendto(t.txFD, frame, 0, &unix.SockaddrLinklayer{Ifindex: t.index, Protocol: htons(unix.ETH_P_IP)})
		if err == nil {
			return nil
		}
		if err != unix.EAGAIN && err != unix.EINTR {
			return err
		}
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := unix.Poll([]unix.PollFd{{Fd: int32(t.txFD), Events: unix.POLLOUT}}, 20); err != nil && err != unix.EINTR {
			return err
		}
	}
}
func (t *nativeTransport) Probe(ctx context.Context, address netip.Addr, clientMAC net.HardwareAddr) (bool, error) {
	if err := t.Check(); err != nil {
		return false, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	if err = unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: t.index, Protocol: htons(unix.ETH_P_ARP)}); err != nil {
		return false, err
	}
	frame := arpProbe(t.mac, address)
	if err = unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: t.index, Protocol: htons(unix.ETH_P_ARP)}); err != nil {
		return false, err
	}
	until := time.Now().Add(time.Duration(t.config.ProbeTimeoutMilliseconds) * time.Millisecond)
	b := make([]byte, 2048)
	for time.Now().Before(until) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, int(max(1, min(50, time.Until(until).Milliseconds())))); err != nil && err != unix.EINTR {
			return false, err
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, _, err := unix.Recvfrom(fd, b, 0)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if arpConflict(b[:n], address, t.mac, clientMAC) {
			return true, nil
		}
	}
	return false, nil
}
func arpProbe(mac net.HardwareAddr, address netip.Addr) []byte {
	b := make([]byte, 42)
	copy(b[:6], broadcastMAC)
	copy(b[6:12], mac)
	binary.BigEndian.PutUint16(b[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(b[14:16], 1)
	binary.BigEndian.PutUint16(b[16:18], unix.ETH_P_IP)
	b[18], b[19] = 6, 4
	binary.BigEndian.PutUint16(b[20:22], 1)
	copy(b[22:28], mac)
	putAddress(b[38:42], address)
	return b
}
func arpConflict(b []byte, address netip.Addr, serverMAC, clientMAC net.HardwareAddr) bool {
	if len(b) < 42 || binary.BigEndian.Uint16(b[12:14]) != unix.ETH_P_ARP || binary.BigEndian.Uint16(b[14:16]) != 1 || binary.BigEndian.Uint16(b[16:18]) != unix.ETH_P_IP || b[18] != 6 || b[19] != 4 {
		return false
	}
	op := binary.BigEndian.Uint16(b[20:22])
	if op != 1 && op != 2 {
		return false
	}
	sender := b[22:28]
	if string(sender) == string(serverMAC) || string(sender) == string(clientMAC) {
		return false
	}
	if sender[0]&1 != 0 || string(sender) == string(make([]byte, 6)) {
		return false
	}
	spa, tpa := addr4(b[28:32]), addr4(b[38:42])
	return spa == address || spa.IsUnspecified() && tpa == address
}
func udpFrame(sourceMAC, destinationMAC net.HardwareAddr, source, destination netip.Addr, sourcePort, destinationPort, id uint16, payload []byte) ([]byte, error) {
	if len(sourceMAC) != 6 || len(destinationMAC) != 6 || !source.Is4() || !destination.Is4() || len(payload) > 1472 {
		return nil, errors.New("invalid DHCP Ethernet reply")
	}
	b := make([]byte, 42+len(payload))
	copy(b[:6], destinationMAC)
	copy(b[6:12], sourceMAC)
	binary.BigEndian.PutUint16(b[12:14], unix.ETH_P_IP)
	ip := b[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(28+len(payload)))
	binary.BigEndian.PutUint16(ip[4:6], id)
	ip[8], ip[9] = 64, 17
	putAddress(ip[12:16], source)
	putAddress(ip[16:20], destination)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	udp := b[34:]
	binary.BigEndian.PutUint16(udp[:2], sourcePort)
	binary.BigEndian.PutUint16(udp[2:4], destinationPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	pseudo := make([]byte, 12+len(udp))
	putAddress(pseudo[:4], source)
	putAddress(pseudo[4:8], destination)
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(udp)))
	copy(pseudo[12:], udp)
	sum := checksum(pseudo)
	if sum == 0 {
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], sum)
	return b, nil
}
func checksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	if len(b) > 0 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
