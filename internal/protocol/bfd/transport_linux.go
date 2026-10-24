//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type processClockReader struct {
	identity atomic.Pointer[string]
	mu       sync.Mutex
	load     func() (string, error)
}

var processClock = processClockReader{load: loadKernelBootID}

// ReadClock observes BOOTTIME on every call. Boot identity is immutable for
// this process's lifetime: a kernel reboot replaces the process, including its
// cache. Avoid filesystem I/O and allocations in BFD/admin deadline hot paths.
func ReadClock() (Clock, error) { return processClock.Read() }

func (r *processClockReader) Read() (Clock, error) {
	id := r.identity.Load()
	if id == nil {
		r.mu.Lock()
		id = r.identity.Load()
		if id == nil {
			value, err := r.load()
			if err == nil && !validBootID(value) {
				err = errors.New("invalid kernel boot ID")
			}
			if err != nil {
				r.mu.Unlock()
				return Clock{}, err
			}
			r.identity.Store(&value)
			id = &value
		}
		r.mu.Unlock()
	}
	return clockForBoot(*id)
}

func loadKernelBootID() (string, error) {
	f, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(value))
	if len(value) > 64 || !validBootID(id) {
		return "", errors.New("invalid kernel boot ID")
	}
	return id, nil
}

func clockForBoot(id string) (Clock, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return Clock{}, err
	}
	value := ts.Nano()
	if value <= 0 {
		return Clock{}, errors.New("invalid CLOCK_BOOTTIME value")
	}
	return Clock{BootID: id, BoottimeNanoseconds: value, WallTime: time.Now().UTC()}, nil
}

type datagram struct {
	data                []byte
	source, destination netip.Addr
	sourcePort          uint16
	index, hopLimit     int
	received            Clock
	invalid             bool
}

type transport interface {
	Receive(context.Context) (datagram, error)
	Send(int, []byte) error
	Verify() error
	Ports() []uint16
	Discriminators() []uint32
	Close() error
}

type nativeTransport struct {
	config         Config
	source         netip.Addr
	sourcePrefix   netip.Prefix
	index, master  int
	linkType       string
	hardware       []byte
	bootID         string
	lastEmpty      Clock
	rx, lock       int
	tx             []int
	ports          []uint16
	discriminators []uint32
	claims         []int
	destinations   []unix.Sockaddr
	links          *netlink.Handle
}

func openTransport(c Config, clock Clock) (_ *nativeTransport, result error) {
	t := &nativeTransport{config: c, source: netip.MustParseAddr(c.SourceAddress), bootID: clock.BootID, lastEmpty: clock, rx: -1, lock: -1}
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
	if err = t.links.SetSocketTimeout(200 * time.Millisecond); err != nil {
		return nil, err
	}
	link, err := t.links.LinkByName(c.Interface)
	if err != nil {
		return nil, err
	}
	a := link.Attrs()
	t.index, t.master, t.linkType, t.hardware = a.Index, a.MasterIndex, link.Type(), append([]byte(nil), a.HardwareAddr...)
	if a.RawFlags&unix.IFF_POINTOPOINT != 0 && !c.PointToPoint {
		return nil, errors.New("point-to-point interface requires explicit BFD point_to_point mode")
	}
	if err = t.Verify(); err != nil {
		return nil, err
	}
	for _, peer := range c.Peers {
		addr := netip.MustParseAddr(peer.Address)
		routes, err := t.links.RouteGetWithOptions(net.IP(addr.AsSlice()), &netlink.RouteGetOptions{OifIndex: t.index, SrcAddr: net.IP(t.source.AsSlice())})
		if err != nil || len(routes) != 1 || routes[0].LinkIndex != t.index || len(routes[0].Gw) != 0 || len(routes[0].MultiPath) != 0 || routes[0].Type != unix.RTN_UNICAST {
			return nil, errors.New("BFD peer requires a direct single-hop route on the configured interface")
		}
		t.destinations = append(t.destinations, t.sockaddr(addr, ControlPort))
	}
	// One process owns all sessions on one interface/family. Do not use UDP
	// SO_REUSEPORT: hash distribution would deliver sessions to the wrong owner.
	t.lock, err = unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err = unix.Bind(t.lock, &unix.SockaddrUnix{Name: fmt.Sprintf("\x00routerd2-bfd:%d:%t", t.index, t.source.Is4())}); err != nil {
		return nil, fmt.Errorf("claim BFD interface/family: %w", err)
	}
	t.rx, err = t.socket()
	if err != nil {
		return nil, err
	}
	if err = unix.SetsockoptInt(t.rx, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS_NEW, 1); err != nil {
		return nil, err
	}
	if t.source.Is4() {
		if err = unix.SetsockoptInt(t.rx, unix.IPPROTO_IP, unix.IP_RECVTTL, 1); err != nil {
			return nil, err
		}
		if err = unix.SetsockoptInt(t.rx, unix.IPPROTO_IP, unix.IP_PKTINFO, 1); err != nil {
			return nil, err
		}
	} else {
		if err = unix.SetsockoptInt(t.rx, unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT, 1); err != nil {
			return nil, err
		}
		if err = unix.SetsockoptInt(t.rx, unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1); err != nil {
			return nil, err
		}
	}
	if err = unix.Bind(t.rx, t.sockaddr(t.source, ControlPort)); err != nil {
		return nil, fmt.Errorf("bind BFD control socket: %w", err)
	}
	for i := range c.Peers {
		discriminator, err := t.claimDiscriminator()
		if err != nil {
			return nil, describePeer(i, err)
		}
		t.discriminators = append(t.discriminators, discriminator)
		fd, err := t.socket()
		if err != nil {
			return nil, err
		}
		t.tx = append(t.tx, fd)
		port, err := t.bindSourcePort(fd)
		if err != nil {
			return nil, describePeer(i, err)
		}
		t.ports = append(t.ports, port)
	}
	return t, nil
}

func (t *nativeTransport) claimDiscriminator() (uint32, error) {
	for attempt := 0; attempt < 128; attempt++ {
		var entropy [4]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return 0, err
		}
		value := binary.BigEndian.Uint32(entropy[:])
		if value == 0 {
			continue
		}
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return 0, err
		}
		err = unix.Bind(fd, &unix.SockaddrUnix{Name: fmt.Sprintf("\x00routerd2-bfd-discriminator:%08x", value)})
		if err == nil {
			t.claims = append(t.claims, fd)
			return value, nil
		}
		_ = unix.Close(fd)
		if !errors.Is(err, unix.EADDRINUSE) {
			return 0, err
		}
	}
	return 0, errors.New("cannot reserve a unique BFD discriminator")
}

func (t *nativeTransport) socket() (int, error) {
	family := unix.AF_INET
	if t.source.Is6() {
		family = unix.AF_INET6
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return -1, err
	}
	fail := func(err error) (int, error) { _ = unix.Close(fd); return -1, err }
	if err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, t.config.Interface); err != nil {
		return fail(err)
	}
	if t.source.Is4() {
		if err = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, 255); err != nil {
			return fail(err)
		}
	} else {
		if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); err != nil {
			return fail(err)
		}
		if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, 255); err != nil {
			return fail(err)
		}
	}
	return fd, nil
}

func (t *nativeTransport) sockaddr(address netip.Addr, port int) unix.Sockaddr {
	if address.Is4() {
		return &unix.SockaddrInet4{Addr: address.As4(), Port: port}
	}
	zone := uint32(0)
	if address.IsLinkLocalUnicast() {
		zone = uint32(t.index)
	}
	return &unix.SockaddrInet6{Addr: address.As16(), Port: port, ZoneId: zone}
}

func (t *nativeTransport) bindSourcePort(fd int) (uint16, error) {
	var entropy [2]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return 0, err
	}
	start := uint32(binary.BigEndian.Uint16(entropy[:])) & 16383
	for attempt := uint32(0); attempt < 16384; attempt++ {
		port := uint16(49152 + (start+attempt)%16384)
		err := unix.Bind(fd, t.sockaddr(t.source, int(port)))
		if err == nil {
			return port, nil
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			return 0, err
		}
	}
	return 0, errors.New("BFD source port range is exhausted")
}

func (t *nativeTransport) Verify() error {
	link, err := t.links.LinkByName(t.config.Interface)
	if err != nil {
		return err
	}
	a := link.Attrs()
	if a.Index != t.index || a.MasterIndex != t.master || link.Type() != t.linkType || !bytes.Equal(a.HardwareAddr, t.hardware) || a.RawFlags&unix.IFF_UP == 0 || a.RawFlags&unix.IFF_LOOPBACK != 0 {
		return errors.New("BFD interface identity, master or administrative state changed")
	}
	family := netlink.FAMILY_V4
	if t.source.Is6() {
		family = netlink.FAMILY_V6
	}
	addresses, err := t.links.AddrList(link, family)
	if err != nil {
		return err
	}
	for _, addr := range addresses {
		ip, ok := netip.AddrFromSlice(addr.IP)
		if ok && ip.Unmap() == t.source && addr.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0 && addr.PreferedLft != 0 && addr.ValidLft != 0 {
			bits, _ := addr.Mask.Size()
			prefix := netip.PrefixFrom(t.source, bits)
			if !prefix.IsValid() || t.sourcePrefix.IsValid() && prefix != t.sourcePrefix {
				return errors.New("BFD source prefix identity changed")
			}
			if !t.config.PointToPoint {
				for _, peer := range t.config.Peers {
					if !prefix.Contains(netip.MustParseAddr(peer.Address)) {
						return errors.New("multiaccess BFD source and peers must belong to the same subnet")
					}
				}
			}
			t.sourcePrefix = prefix
			return nil
		}
	}
	return errors.New("BFD configured source is not a usable address on its interface")
}

func (t *nativeTransport) Ports() []uint16 { return append([]uint16(nil), t.ports...) }
func (t *nativeTransport) Discriminators() []uint32 {
	return append([]uint32(nil), t.discriminators...)
}
func (t *nativeTransport) Send(peer int, data []byte) error {
	return unix.Sendto(t.tx[peer], data, unix.MSG_DONTROUTE, t.destinations[peer])
}

func (t *nativeTransport) Receive(ctx context.Context) (datagram, error) {
	buffer, oob := make([]byte, 4096), make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return datagram{}, err
		}
		before, err := clockForBoot(t.bootID)
		if err != nil {
			return datagram{}, err
		}
		n, cn, flags, from, err := unix.Recvmsg(t.rx, buffer, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				// This sample precedes a verified empty receive queue. Every
				// future queued datagram arrived after this BOOTTIME lower bound.
				t.lastEmpty = before
				poll := []unix.PollFd{{Fd: int32(t.rx), Events: unix.POLLIN}}
				if _, err := unix.Poll(poll, 5); err != nil && !errors.Is(err, unix.EINTR) {
					return datagram{}, err
				}
				if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
					return datagram{}, errors.New("BFD receive socket failed")
				}
				continue
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return datagram{}, err
		}
		clock, err := clockForBoot(t.bootID)
		if err != nil {
			return datagram{}, err
		}
		p := datagram{data: append([]byte(nil), buffer[:min(n, len(buffer))]...), invalid: flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n > len(buffer)}
		switch addr := from.(type) {
		case *unix.SockaddrInet4:
			p.source, p.sourcePort = netip.AddrFrom4(addr.Addr), uint16(addr.Port)
		case *unix.SockaddrInet6:
			p.source, p.sourcePort = netip.AddrFrom16(addr.Addr), uint16(addr.Port)
		default:
			p.invalid = true
		}
		messages, err := unix.ParseSocketControlMessage(oob[:cn])
		if err != nil {
			p.invalid = true
			return p, nil
		}
		var timestamp time.Time
		for _, cm := range messages {
			switch {
			case cm.Header.Level == unix.SOL_SOCKET && cm.Header.Type == unix.SO_TIMESTAMPNS_NEW && len(cm.Data) == 16:
				seconds, nanos := int64(binary.NativeEndian.Uint64(cm.Data[:8])), int64(binary.NativeEndian.Uint64(cm.Data[8:]))
				if nanos >= 0 && nanos < 1_000_000_000 {
					timestamp = time.Unix(seconds, nanos).UTC()
				}
			case cm.Header.Level == unix.IPPROTO_IP && cm.Header.Type == unix.IP_TTL && len(cm.Data) == 4:
				p.hopLimit = int(binary.NativeEndian.Uint32(cm.Data))
			case cm.Header.Level == unix.IPPROTO_IP && cm.Header.Type == unix.IP_PKTINFO && len(cm.Data) == 12:
				p.index = int(binary.NativeEndian.Uint32(cm.Data[:4]))
				p.destination = netip.AddrFrom4([4]byte(cm.Data[8:12]))
			case cm.Header.Level == unix.IPPROTO_IPV6 && cm.Header.Type == unix.IPV6_HOPLIMIT && len(cm.Data) == 4:
				p.hopLimit = int(binary.NativeEndian.Uint32(cm.Data))
			case cm.Header.Level == unix.IPPROTO_IPV6 && cm.Header.Type == unix.IPV6_PKTINFO && len(cm.Data) == 20:
				p.index = int(binary.NativeEndian.Uint32(cm.Data[16:20]))
				p.destination = netip.AddrFrom16([16]byte(cm.Data[:16]))
			}
		}
		p.received, err = receiveClock(clock, t.lastEmpty, timestamp)
		if err != nil {
			p.invalid = true
		}
		if p.index != t.index || p.destination != t.source || p.hopLimit != 255 || p.sourcePort < 49152 {
			p.invalid = true
		}
		return p, nil
	}
}

// Linux SO_TIMESTAMPNS_NEW is realtime, so directly subtracting queue age from
// BOOTTIME can renew an old packet after a backward wall-clock step. Bound the
// conversion by the last verified empty-queue BOOTTIME sample. The bound is
// conservative (normally at most the 5ms poll interval plus scheduling delay)
// and remains safe through suspension and arbitrary realtime adjustments.
func receiveClock(now, empty Clock, timestamp time.Time) (Clock, error) {
	age := now.WallTime.Sub(timestamp)
	if !validWall(timestamp) || age < 0 || int64(age) >= now.BoottimeNanoseconds || empty.BootID != now.BootID || empty.BoottimeNanoseconds <= 0 || empty.BoottimeNanoseconds > now.BoottimeNanoseconds {
		return Clock{}, errors.New("BFD kernel receive timestamp cannot be safely scoped to BOOTTIME")
	}
	received := min(now.BoottimeNanoseconds-int64(age), empty.BoottimeNanoseconds)
	return Clock{BootID: now.BootID, BoottimeNanoseconds: received, WallTime: timestamp}, nil
}

func (t *nativeTransport) Close() error {
	var errs []error
	for _, fd := range append(append([]int{t.rx, t.lock}, t.tx...), t.claims...) {
		if fd >= 0 {
			errs = append(errs, unix.Close(fd))
		}
	}
	if t.links != nil {
		t.links.Close()
	}
	t.rx, t.lock, t.tx, t.claims = -1, -1, nil, nil
	return errors.Join(errs...)
}
