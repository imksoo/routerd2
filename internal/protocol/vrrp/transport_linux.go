//go:build linux

package vrrp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var errSourcePending = errors.New("VRRP IPv6 source address is pending")

type nativeTransport struct {
	config    Config
	index     int
	rxFD      int
	txFD      int
	unicastFD int
	lockFD    int
	links     *netlink.Handle
	source    netip.Addr
}

func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

func openTransport(ctx context.Context, c Config) (_ *nativeTransport, _ netip.Addr, result error) {
	t := &nativeTransport{config: c, rxFD: -1, txFD: -1, unicastFD: -1, lockFD: -1}
	defer func() {
		if result != nil {
			_ = t.Close()
		}
	}()
	var err error
	t.links, err = netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	if err := t.links.SetSocketTimeout(min(250*time.Millisecond, c.interval())); err != nil {
		return nil, netip.Addr{}, err
	}
	link, err := t.links.LinkByName(c.Interface)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	attrs := link.Attrs()
	if len(attrs.HardwareAddr) != 6 || attrs.RawFlags&unix.IFF_UP == 0 || len(c.UnicastPeers) == 0 && attrs.RawFlags&unix.IFF_MULTICAST == 0 {
		return nil, netip.Addr{}, errors.New("VRRP election interface must be an up Ethernet link with multicast support when required")
	}
	t.index = attrs.Index
	if c.VRF != "" {
		vrf, err := t.links.LinkByName(c.VRF)
		if err != nil || vrf.Type() != "vrf" {
			return nil, netip.Addr{}, errors.New("VRRP configured VRF does not exist or is not a VRF")
		}
		master := attrs.MasterIndex
		for depth := 0; depth < 16 && master != 0 && master != vrf.Attrs().Index; depth++ {
			next, err := t.links.LinkByIndex(master)
			if err != nil {
				return nil, netip.Addr{}, err
			}
			master = next.Attrs().MasterIndex
		}
		if master != vrf.Attrs().Index {
			return nil, netip.Addr{}, errors.New("VRRP election interface is not in the configured VRF")
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, netip.Addr{}, err
		}
		t.source, err = chooseSource(t.links, link, c)
		if !errors.Is(err, errSourcePending) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, netip.Addr{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, netip.Addr{}, err
	}
	header := 20
	if c.Family == "ipv6" {
		header = 40
	}
	if header+advertisementSize(c, len(c.Addresses)) > attrs.MTU {
		return nil, netip.Addr{}, errors.New("VRRP address list exceeds the election interface MTU")
	}
	// Abstract Unix addresses are scoped to the network namespace. This
	// process-lifetime lock rejects duplicate interface/family/VRID instances
	// without lock files or stale ownership after SIGKILL.
	t.lockFD, err = unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	name := fmt.Sprintf("\x00routerd2-vrrp:%d:%s:%d", t.index, c.Family, c.VirtualRouterID)
	if err := unix.Bind(t.lockFD, &unix.SockaddrUnix{Name: name}); err != nil {
		return nil, netip.Addr{}, fmt.Errorf("claim VRRP instance: %w", err)
	}
	family := unix.AF_INET
	if c.Family == "ipv6" {
		family = unix.AF_INET6
	}
	t.rxFD, err = unix.Socket(family, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return nil, netip.Addr{}, fmt.Errorf("open VRRP raw receive socket: %w", err)
	}
	if err := unix.SetsockoptString(t.rxFD, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface); err != nil {
		return nil, netip.Addr{}, err
	}
	if c.Family == "ipv4" {
		if err := unix.SetsockoptInt(t.rxFD, unix.IPPROTO_IP, unix.IP_PKTINFO, 1); err != nil {
			return nil, netip.Addr{}, err
		}
		if len(c.UnicastPeers) == 0 {
			membership := &unix.IPMreqn{Multiaddr: group4.As4(), Ifindex: int32(t.index)}
			if err := unix.SetsockoptIPMreqn(t.rxFD, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, membership); err != nil {
				return nil, netip.Addr{}, err
			}
		}
	} else {
		for _, option := range []int{unix.IPV6_RECVPKTINFO, unix.IPV6_RECVHOPLIMIT} {
			if err := unix.SetsockoptInt(t.rxFD, unix.IPPROTO_IPV6, option, 1); err != nil {
				return nil, netip.Addr{}, err
			}
		}
		if len(c.UnicastPeers) == 0 {
			membership := &unix.IPv6Mreq{Multiaddr: group6.As16(), Interface: uint32(t.index)}
			if err := unix.SetsockoptIPv6Mreq(t.rxFD, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, membership); err != nil {
				return nil, netip.Addr{}, err
			}
		}
	}
	// Multicast group membership above also emits IGMP/MLD as needed. Multicast
	// transmission uses Ethernet frames because an ordinary IP raw socket uses the
	// physical link's MAC instead of the protocol's mandatory virtual MAC.
	t.txFD, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, netip.Addr{}, fmt.Errorf("open VRRP raw transmit socket: %w", err)
	}
	if err := unix.Bind(t.txFD, &unix.SockaddrLinklayer{Ifindex: t.index}); err != nil {
		return nil, netip.Addr{}, err
	}
	if len(c.UnicastPeers) != 0 {
		if err := t.openUnicast(family); err != nil {
			return nil, netip.Addr{}, err
		}
	}
	return t, t.source, nil
}

func chooseSource(handle *netlink.Handle, link netlink.Link, c Config) (netip.Addr, error) {
	family := netlink.FAMILY_V4
	if c.Family == "ipv6" {
		family = netlink.FAMILY_V6
	}
	addresses, err := handle.AddrList(link, family)
	if err != nil {
		return netip.Addr{}, err
	}
	var candidates []netip.Addr
	pending := c.Family == "ipv6" && c.SourceAddress == ""
	for _, address := range addresses {
		if address.Flags&unix.IFA_F_DADFAILED != 0 {
			continue
		}
		a, ok := netip.AddrFromSlice(address.IP)
		if !ok {
			continue
		}
		a = a.Unmap()
		if !addressInFamily(a, c.Family) || len(c.UnicastPeers) == 0 && c.Family == "ipv6" && !a.IsLinkLocalUnicast() {
			continue
		}
		if c.SourceAddress != "" && a.String() != c.SourceAddress {
			continue
		}
		if address.Flags&unix.IFA_F_TENTATIVE != 0 {
			pending = true
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Less(candidates[j]) })
		return candidates[0], nil
	}
	if pending {
		return netip.Addr{}, errSourcePending
	}
	if c.SourceAddress != "" {
		// An explicit IPv4 address owner can move its real address onto the
		// VMAC link as part of the acknowledged activation. VerifyActive checks
		// its actual presence before any packet is sent.
		if c.AddressOwner && c.Family == "ipv4" {
			source := netip.MustParseAddr(c.SourceAddress)
			for _, vip := range c.addresses() {
				if source == vip {
					return source, nil
				}
			}
		}
		return netip.Addr{}, errors.New("VRRP source address is not ready on the election interface")
	}
	return netip.Addr{}, errors.New("VRRP election interface has no usable source address")
}

func pollSocket(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		milliseconds := 200
		if deadline, ok := ctx.Deadline(); ok {
			left := time.Until(deadline)
			if left <= 0 {
				return context.DeadlineExceeded
			}
			milliseconds = min(milliseconds, max(1, int((left+time.Millisecond-1)/time.Millisecond)))
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, milliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if fds[0].Revents&(unix.POLLNVAL|unix.POLLHUP) != 0 {
				return errors.New("VRRP socket closed")
			}
			return nil
		}
	}
}

func (t *nativeTransport) Receive(ctx context.Context) (datagram, error) {
	buffer, oob := make([]byte, 65536), make([]byte, 256)
	for {
		if err := pollSocket(ctx, t.rxFD, unix.POLLIN); err != nil {
			return datagram{}, err
		}
		n, controlLen, flags, sender, err := unix.Recvmsg(t.rxFD, buffer, oob, 0)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return datagram{}, err
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n == 0 {
			return datagram{}, nil // Let the runtime count a malformed datagram.
		}
		messages, err := unix.ParseSocketControlMessage(oob[:controlLen])
		if err != nil {
			return datagram{}, nil
		}
		index, hop := 0, -1
		var destination netip.Addr
		for _, message := range messages {
			if message.Header.Level == unix.IPPROTO_IP && message.Header.Type == unix.IP_PKTINFO && len(message.Data) >= 12 {
				index = int(binary.NativeEndian.Uint32(message.Data[:4]))
				destination = netip.AddrFrom4([4]byte(message.Data[8:12]))
			}
			if message.Header.Level == unix.IPPROTO_IPV6 && message.Header.Type == unix.IPV6_PKTINFO && len(message.Data) >= 20 {
				index = int(binary.NativeEndian.Uint32(message.Data[16:20]))
				destination = netip.AddrFrom16([16]byte(message.Data[:16]))
			}
			if message.Header.Level == unix.IPPROTO_IPV6 && message.Header.Type == unix.IPV6_HOPLIMIT && len(message.Data) >= 4 {
				hop = int(binary.NativeEndian.Uint32(message.Data[:4]))
			}
		}
		if index != t.index {
			return datagram{}, nil
		}
		if t.config.Family == "ipv4" {
			// Linux validates/reassembles the IP packet before raw delivery.
			// Preserve its actual endpoints, TTL and payload for VRRP checks.
			if n < 20 {
				return datagram{}, nil
			}
			header := int(buffer[0]&15) * 4
			if header < 20 || header > n {
				return datagram{}, nil
			}
			return datagram{source: netip.AddrFrom4([4]byte(buffer[12:16])), destination: destination, hopLimit: int(buffer[8]), payload: buffer[header:n]}, nil
		}
		address, ok := sender.(*unix.SockaddrInet6)
		if !ok {
			return datagram{}, nil
		}
		return datagram{source: netip.AddrFrom16(address.Addr), destination: destination, hopLimit: hop, payload: buffer[:n]}, nil
	}
}

func (t *nativeTransport) SendFrame(ctx context.Context, frame []byte) error {
	if len(frame) < 14 {
		return errors.New("cannot send incomplete Ethernet frame")
	}
	destination := &unix.SockaddrLinklayer{Ifindex: t.index, Protocol: htons(binary.BigEndian.Uint16(frame[12:14])), Halen: 6}
	copy(destination.Addr[:], frame[:6])
	return sendTo(ctx, t.txFD, frame, destination)
}

func sendTo(ctx context.Context, fd int, payload []byte, destination unix.Sockaddr) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Sendto(fd, payload, 0, destination)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOBUFS) {
			if err := pollSocket(ctx, fd, unix.POLLOUT); err != nil {
				return err
			}
			continue
		}
		return err
	}
}

func (t *nativeTransport) VerifyActive(ctx context.Context, snapshot Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	link, err := t.links.LinkByName(t.config.VIPInterface)
	if err != nil {
		return err
	}
	if link.Type() != "macvlan" || link.Attrs().ParentIndex != t.index || link.Attrs().RawFlags&unix.IFF_UP == 0 || !bytes.Equal(link.Attrs().HardwareAddr, VirtualMAC(t.config.Family, t.config.VirtualRouterID)) {
		return errors.New("VRRP VIP link must be an up macvlan on the election interface with the configured virtual MAC")
	}
	if t.config.VRF != "" {
		vrf, err := t.links.LinkByName(t.config.VRF)
		if err != nil || vrf.Type() != "vrf" || link.Attrs().MasterIndex != vrf.Attrs().Index {
			return errors.New("VRRP virtual interface is not in the configured VRF")
		}
	}
	family := netlink.FAMILY_V4
	if t.config.Family == "ipv6" {
		family = netlink.FAMILY_V6
	}
	addresses, err := t.links.AddrList(link, family)
	if err != nil {
		return err
	}
	ready := make(map[netip.Prefix]bool, len(addresses))
	for _, address := range addresses {
		if address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			if addr, ok := netip.AddrFromSlice(address.IP); ok {
				bits, _ := address.Mask.Size()
				ready[netip.PrefixFrom(addr.Unmap(), bits)] = true
			}
		}
	}
	for _, value := range t.config.Addresses {
		address := netip.MustParsePrefix(value)
		if !ready[address] {
			return fmt.Errorf("VRRP virtual address %s is not ready on its virtual interface", address)
		}
	}
	parent, err := t.links.LinkByIndex(t.index)
	if err != nil || parent.Attrs().Name != t.config.Interface || parent.Attrs().RawFlags&unix.IFF_UP == 0 {
		return errors.New("VRRP election interface is no longer up")
	}
	sourceConfig := t.config
	sourceConfig.SourceAddress = t.source.String()
	if _, err := chooseSource(t.links, parent, sourceConfig); err != nil {
		return err
	}
	if !snapshot.Active || !time.Now().Before(snapshot.ValidUntil) {
		return errors.New("VRRP ownership lease is not active")
	}
	return ctx.Err()
}

func (t *nativeTransport) Close() error {
	var errs []error
	for _, fd := range []int{t.rxFD, t.txFD, t.unicastFD, t.lockFD} {
		if fd >= 0 {
			errs = append(errs, unix.Close(fd))
		}
	}
	t.rxFD, t.txFD, t.unicastFD, t.lockFD = -1, -1, -1, -1
	if t.links != nil {
		t.links.Close()
		t.links = nil
	}
	return errors.Join(errs...)
}
