//go:build linux

package vrrp

import (
	"context"

	"golang.org/x/sys/unix"
)

func (t *nativeTransport) openUnicast(family int) error {
	fd, err := unix.Socket(family, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return err
	}
	t.unicastFD = fd
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, t.config.Interface); err != nil {
		return err
	}
	if family == unix.AF_INET {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, 255); err != nil {
			return err
		}
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TOS, 0xc0); err != nil {
			return err
		}
		// An explicit address owner's source can be installed by the first
		// ownership ACK. VerifyActive still precedes all positive adverts.
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_FREEBIND, 1); err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrInet4{Addr: t.source.As4()})
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, 255); err != nil {
		return err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_FREEBIND, 1); err != nil {
		return err
	}
	return unix.Bind(fd, &unix.SockaddrInet6{Addr: t.source.As16(), ZoneId: uint32(t.index)})
}

func (t *nativeTransport) Advertise(ctx context.Context, p datagram) error {
	if len(t.config.UnicastPeers) == 0 {
		return t.SendFrame(ctx, datagramFrame(t.config.Family, t.config.VirtualRouterID, p))
	}
	// The IP socket owns routing and ARP/ND resolution. No neighbor cache or
	// fabricated Ethernet destination is needed for a unicast peer.
	if p.destination.Is4() {
		return sendTo(ctx, t.unicastFD, p.payload, &unix.SockaddrInet4{Addr: p.destination.As4()})
	}
	return sendTo(ctx, t.unicastFD, p.payload, &unix.SockaddrInet6{Addr: p.destination.As16(), ZoneId: uint32(t.index)})
}
