// SPDX-License-Identifier: BSD-3-Clause
// Adapted from imksoo/routerd@1329ef54f296507eda79bbbc3b853783dd27251e
// cmd/routerd-dhcpv4-client/socket_linux.go. See NOTICE.

//go:build linux

package dhcp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	etherTypeIPv4 = 0x0800
	ethHeaderLen  = 14
)

func bindSocketToDevice(fd int, ifname string) error {
	return unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifname)
}

func listenDHCPv4(ifname string) (packetConn, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	rawFD, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(etherTypeIPv4)))
	if err != nil {
		return nil, err
	}
	timeout := unix.NsecToTimeval(int64(time.Second))
	if err := unix.SetsockoptTimeval(rawFD, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
		unix.Close(rawFD)
		return nil, err
	}
	if err := attachDHCPv4Filter(rawFD); err != nil {
		_ = unix.Close(rawFD)
		return nil, err
	}
	// AF_PACKET may see CHECKSUM_PARTIAL on virtual links before software
	// checksum completion. Only kernel-supplied auxdata may exempt that UDP
	// checksum from validation; packet bytes themselves cannot opt out.
	if err := unix.SetsockoptInt(rawFD, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1); err != nil {
		_ = unix.Close(rawFD)
		return nil, err
	}
	if err := unix.Bind(rawFD, &unix.SockaddrLinklayer{Protocol: htons(etherTypeIPv4), Ifindex: ifi.Index}); err != nil {
		_ = unix.Close(rawFD)
		return nil, err
	}
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if sockErr == nil {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			}
			if sockErr == nil && ifname != "" {
				sockErr = bindSocketToDevice(int(fd), ifname)
			}
		}); err != nil && sockErr == nil {
			sockErr = err
		}
		return sockErr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":68")
	if err != nil {
		_ = unix.Close(rawFD)
		return nil, err
	}
	return &linuxDHCPv4PacketConn{rawFD: rawFD, write: pc.(*net.UDPConn), ifindex: ifi.Index, hwaddr: append(net.HardwareAddr(nil), ifi.HardwareAddr...)}, nil
}

func attachDHCPv4Filter(fd int) error {
	filter := []unix.SockFilter{
		bpfStmt(unix.BPF_LD|unix.BPF_H|unix.BPF_ABS, 12),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, etherTypeIPv4, 0, 13),
		bpfStmt(unix.BPF_LD|unix.BPF_B|unix.BPF_ABS, 23),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.IPPROTO_UDP, 0, 11),
		bpfStmt(unix.BPF_LD|unix.BPF_H|unix.BPF_ABS, 20),
		bpfJump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, 0x1fff, 9, 0),
		bpfStmt(unix.BPF_LD|unix.BPF_B|unix.BPF_ABS, 14),
		bpfStmt(unix.BPF_ALU|unix.BPF_AND|unix.BPF_K, 0x0f),
		bpfStmt(unix.BPF_ALU|unix.BPF_LSH|unix.BPF_K, 2),
		bpfStmt(unix.BPF_MISC|unix.BPF_TAX, 0),
		bpfStmt(unix.BPF_LD|unix.BPF_H|unix.BPF_IND, 14),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, 67, 0, 3),
		bpfStmt(unix.BPF_LD|unix.BPF_H|unix.BPF_IND, 16),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, 68, 0, 1),
		bpfStmt(unix.BPF_RET|unix.BPF_K, 0xffff),
		bpfStmt(unix.BPF_RET|unix.BPF_K, 0),
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	return unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &program)
}

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

type linuxDHCPv4PacketConn struct {
	rawFD    int
	write    *net.UDPConn
	hwaddr   net.HardwareAddr
	ifindex  int
	deadline time.Time
}

func (c *linuxDHCPv4PacketConn) ReadFromUDP(buf []byte) (int, *net.UDPAddr, error) {
	frame := make([]byte, 65535+ethHeaderLen)
	oob := make([]byte, 128)
	for {
		if !c.deadline.IsZero() && !time.Now().Before(c.deadline) {
			return 0, nil, os.ErrDeadlineExceeded
		}
		n, control, flags, _, err := unix.Recvmsg(c.rawFD, frame, oob, 0)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return 0, nil, linuxTimeoutError{err: err}
			}
			return 0, nil, os.NewSyscallError("recvfrom", err)
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			continue
		}
		cms, err := unix.ParseSocketControlMessage(oob[:control])
		if err != nil {
			continue
		}
		checksumPartial := false
		for _, cm := range cms {
			if cm.Header.Level == unix.SOL_PACKET && cm.Header.Type == unix.PACKET_AUXDATA && len(cm.Data) >= 20 {
				checksumPartial = binary.NativeEndian.Uint32(cm.Data)&unix.TP_STATUS_CSUMNOTREADY != 0
			}
		}
		payload, src := c.dhcpPayloadWithChecksumState(frame[:n], checksumPartial)
		if len(payload) == 0 {
			continue
		}
		copied := copy(buf, payload)
		return copied, &net.UDPAddr{IP: src, Port: 67}, nil
	}
}

type linuxTimeoutError struct {
	err error
}

func (e linuxTimeoutError) Error() string {
	return e.err.Error()
}

func (e linuxTimeoutError) Timeout() bool {
	return true
}

func (e linuxTimeoutError) Temporary() bool {
	return true
}

func (e linuxTimeoutError) Unwrap() error {
	return e.err
}

func (c *linuxDHCPv4PacketConn) dhcpPayload(frame []byte) ([]byte, net.IP) {
	return c.dhcpPayloadWithChecksumState(frame, false)
}

func (c *linuxDHCPv4PacketConn) dhcpPayloadWithChecksumState(frame []byte, checksumPartial bool) ([]byte, net.IP) {
	if len(frame) < ethHeaderLen+20 {
		return nil, nil
	}
	if len(c.hwaddr) > 0 && len(frame) >= 12 && net.HardwareAddr(frame[6:12]).String() == c.hwaddr.String() {
		return nil, nil
	}
	if binary.BigEndian.Uint16(frame[12:14]) != etherTypeIPv4 {
		return nil, nil
	}
	ip := frame[ethHeaderLen:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl+8 || ip[9] != unix.IPPROTO_UDP || binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
		return nil, nil
	}
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if total < ihl+8 || total > len(ip) {
		return nil, nil
	}
	ip = ip[:total]
	if checksum(ip[:ihl]) != 0 {
		return nil, nil
	}
	udp := ip[ihl:]
	if len(udp) < 8 {
		return nil, nil
	}
	srcPort := binary.BigEndian.Uint16(udp[0:2])
	dstPort := binary.BigEndian.Uint16(udp[2:4])
	if srcPort != 67 || dstPort != 68 {
		return nil, nil
	}
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		return nil, nil
	}
	if !checksumPartial && binary.BigEndian.Uint16(udp[6:8]) != 0 {
		pseudo := make([]byte, 12+udpLen)
		copy(pseudo[:8], ip[12:20])
		pseudo[9] = unix.IPPROTO_UDP
		binary.BigEndian.PutUint16(pseudo[10:12], uint16(udpLen))
		copy(pseudo[12:], udp[:udpLen])
		if checksum(pseudo) != 0 {
			return nil, nil
		}
	}
	return udp[8:udpLen], net.IPv4(ip[12], ip[13], ip[14], ip[15])
}

func (c *linuxDHCPv4PacketConn) WriteToUDP(buf []byte, addr *net.UDPAddr) (int, error) {
	if len(buf) < 240 || addr == nil || addr.Port != 67 || addr.IP.To4() == nil {
		return 0, fmt.Errorf("invalid DHCPv4 outbound packet")
	}
	if binary.BigEndian.Uint32(buf[12:16]) == 0 {
		// INIT/SELECTING must work before any IPv4 address or route exists.
		// UDP socket routing cannot reliably source 0.0.0.0 on an already
		// configured link, so send that broadcast directly as Ethernet.
		frame := initialFrame(c.hwaddr, buf)
		to := &unix.SockaddrLinklayer{Ifindex: c.ifindex, Protocol: htons(etherTypeIPv4), Halen: 6}
		for i := 0; i < 6; i++ {
			to.Addr[i] = 255
		}
		if err := unix.Sendto(c.rawFD, frame, 0, to); err != nil {
			return 0, err
		}
		return len(buf), nil
	}
	// RENEW/REBIND uses the lease address even if another IPv4 address is
	// present on the interface. The kernel handles ARP for unicast renewal.
	info := &unix.Inet4Pktinfo{Ifindex: int32(c.ifindex)}
	copy(info.Spec_dst[:], buf[12:16])
	if err := c.write.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return 0, err
	}
	n, _, err := c.write.WriteMsgUDP(buf, unix.PktInfo4(info), addr)
	return n, err
}

func initialFrame(hardware net.HardwareAddr, payload []byte) []byte {
	f := make([]byte, 14+20+8+len(payload))
	for i := 0; i < 6; i++ {
		f[i] = 255
	}
	copy(f[6:12], hardware)
	binary.BigEndian.PutUint16(f[12:14], etherTypeIPv4)
	ip := f[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(f)-14))
	ip[8] = 64
	ip[9] = unix.IPPROTO_UDP
	copy(ip[16:20], []byte{255, 255, 255, 255})
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	udp := f[34:]
	binary.BigEndian.PutUint16(udp[0:2], 68)
	binary.BigEndian.PutUint16(udp[2:4], 67)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	return f
}
func checksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}

func (c *linuxDHCPv4PacketConn) SetReadDeadline(deadline time.Time) error {
	c.deadline = deadline
	var timeout time.Duration
	if deadline.IsZero() {
		timeout = 0
	} else {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Microsecond
		}
	}
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	return unix.SetsockoptTimeval(c.rawFD, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

func (c *linuxDHCPv4PacketConn) Close() error {
	err1 := unix.Close(c.rawFD)
	err2 := c.write.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func htons(v uint16) uint16 {
	return (v<<8)&0xff00 | v>>8
}
