//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func nowInstant() (instant, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return instant{}, err
	}
	return instant{wall: time.Now().UTC(), boot: time.Unix(ts.Sec, ts.Nsec).UTC()}, nil
}

// Run calls ready only after the raw socket, journal and initial full snapshot
// are available. No router/lease is required for readiness. Callback, durable
// state, identity and transport failures are fatal; malformed packets are
// counted and discarded atomically. Cancellation publishes Active=false through
// a bounded cleanup context while preserving the journal for same-boot restart.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("RA client effects are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := lockState(cfg.StatePath)
	if err != nil {
		return err
	}
	defer lock.Close()
	link, err := inspectLink(cfg)
	if err != nil {
		return err
	}
	if err = checkKernelOwnership(cfg.Interface); err != nil {
		return err
	}
	s, err := newState(cfg, link)
	if err != nil {
		return err
	}
	bootID, err := readBootID()
	if err != nil {
		return err
	}
	now, err := nowInstant()
	if err != nil {
		return err
	}
	if err = s.restore(cfg.StatePath, bootID, now); err != nil {
		return err
	}
	fd, pktinfo, err := openSocket(cfg, link)
	if err != nil {
		return fmt.Errorf("open RA raw socket: %w", err)
	}
	defer unix.Close(fd)
	publish := func(now instant) error {
		snap := s.snapshot(now)
		if err := ValidateSnapshot(cfg, snap); err != nil {
			return fmt.Errorf("RA client produced invalid snapshot: %w", err)
		}
		if err := saveState(cfg.StatePath, s.record(bootID, now)); err != nil {
			return fmt.Errorf("persist RA state: %w", err)
		}
		if err := effects.Update(ctx, snap); err != nil {
			return fmt.Errorf("publish RA state: %w", err)
		}
		return nil
	}
	if err = publish(now); err != nil {
		return err
	}
	defer func() {
		now, err := nowInstant()
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		snap := s.snapshot(now)
		snap.Active = false
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := effects.Update(cleanup, snap); err != nil {
			result = errors.Join(result, fmt.Errorf("withdraw RA client effects: %w", err))
		}
	}()
	if ready != nil {
		ready()
	}
	nextSolicit := now.boot.Add(solicitationDelay())
	tries := 0
	lastPublish, nextInspect := now.boot, now.boot.Add(time.Second)
	inspectConfig := cfg
	inspectConfig.SourceAddress = link.source.String()
	dirty := false
	buf, oob := make([]byte, 65535), make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now, err = nowInstant()
		if err != nil {
			return err
		}
		hadRoutes := len(s.learned.Routes) > 0
		if s.expire(now) || wallClockMoved(s.clock, now) {
			if hadRoutes && len(s.learned.Routes) == 0 {
				tries = 0
				nextSolicit = now.boot.Add(solicitationDelay())
			}
			if err := publish(now); err != nil {
				return err
			}
			lastPublish, dirty = now.boot, false
		}
		if !now.boot.Before(nextInspect) {
			observed, err := inspectLink(inspectConfig)
			if err != nil {
				return err
			}
			if observed.index != link.index || observed.source != link.source || string(observed.hardware) != string(link.hardware) {
				return errors.New("RA receive link/source identity changed; restart with a fresh ownership generation")
			}
			s.link.mtu = observed.mtu
			if err := checkKernelOwnership(cfg.Interface); err != nil {
				return err
			}
			l, err := netlink.LinkByName(cfg.Interface)
			if err != nil {
				return err
			}
			addresses, err := netlink.AddrList(l, netlink.FAMILY_V6)
			if err != nil {
				return err
			}
			for _, a := range addresses {
				if a.Flags&unix.IFA_F_DADFAILED != 0 {
					ip, ok := netip.AddrFromSlice(a.IP)
					if ok {
						dirty = s.dadFailed(netip.PrefixFrom(ip, 128).String()) || dirty
					}
				}
			}
			nextInspect = now.boot.Add(time.Second)
		}
		if !nextSolicit.IsZero() && !now.boot.Before(nextSolicit) {
			if err := sendSolicitation(fd, pktinfo, link); err != nil {
				return err
			}
			s.learned.Solicitations++
			tries++
			dirty = true
			if tries < 3 {
				nextSolicit = now.boot.Add(4 * time.Second)
			} else {
				nextSolicit = time.Time{}
			}
		}
		if dirty && !now.boot.Before(lastPublish.Add(time.Second)) {
			if err := publish(now); err != nil {
				return err
			}
			lastPublish, dirty = now.boot, false
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 100); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return errors.New("RA receive socket failed")
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, control, flags, from, err := unix.Recvmsg(fd, buf, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return err
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			s.learned.Rejected++
			s.learned.LastError = "invalid router advertisement: truncated datagram/control data"
			dirty = true
			continue
		}
		source, ok := from.(*unix.SockaddrInet6)
		if !ok {
			s.learned.Rejected++
			s.learned.LastError = "invalid router advertisement: unavailable IPv6 source metadata"
			dirty = true
			continue
		}
		p := packet{payload: buf[:n], source: netip.AddrFrom16(source.Addr)}
		cms, err := unix.ParseSocketControlMessage(oob[:control])
		if err != nil {
			s.learned.Rejected++
			s.learned.LastError = "invalid router advertisement: malformed ancillary metadata"
			dirty = true
			continue
		}
		for _, cm := range cms {
			if cm.Header.Level != unix.IPPROTO_IPV6 {
				continue
			}
			if cm.Header.Type == unix.IPV6_HOPLIMIT && len(cm.Data) >= 4 {
				p.hopLimit = int(binary.NativeEndian.Uint32(cm.Data))
			}
			if cm.Header.Type == unix.IPV6_PKTINFO && len(cm.Data) >= 20 {
				var dst [16]byte
				copy(dst[:], cm.Data[:16])
				p.destination = netip.AddrFrom16(dst)
				p.ifindex = int(binary.NativeEndian.Uint32(cm.Data[16:]))
			}
		}
		now, err = nowInstant()
		if err != nil {
			return err
		}
		if err := s.handle(p, now); err != nil {
			dirty = true
			continue
		}
		nextSolicit = time.Time{}
		if err := publish(now); err != nil {
			return err
		}
		lastPublish, dirty = now.boot, false
	}
}

func checkKernelOwnership(ifname string) error {
	for _, key := range []string{"accept_ra", "autoconf"} {
		b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + ifname + "/" + key)
		if err != nil {
			return fmt.Errorf("read interface IPv6 %s: %w", key, err)
		}
		if strings.TrimSpace(string(b)) != "0" {
			return fmt.Errorf("RA client requires the single network writer to own %s=0 on its interface", key)
		}
	}
	return nil
}

func inspectLink(c Config) (linkInfo, error) {
	ifi, err := net.InterfaceByName(c.Interface)
	if err != nil {
		return linkInfo{}, err
	}
	if ifi.Flags&net.FlagUp == 0 {
		return linkInfo{}, errors.New("RA receive interface is not administratively up")
	}
	l, err := netlink.LinkByName(c.Interface)
	if err != nil {
		return linkInfo{}, err
	}
	addresses, err := netlink.AddrList(l, netlink.FAMILY_V6)
	if err != nil {
		return linkInfo{}, err
	}
	want, _ := netip.ParseAddr(c.SourceAddress)
	for _, a := range addresses {
		ip, ok := netip.AddrFromSlice(a.IP)
		if ok && ip.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 && (!want.IsValid() || want == ip) {
			return linkInfo{index: ifi.Index, mtu: ifi.MTU, hardware: append(net.HardwareAddr(nil), ifi.HardwareAddr...), source: ip}, nil
		}
	}
	return linkInfo{}, errors.New("RA receive interface has no usable link-local source")
}

func openSocket(c Config, link linkInfo) (int, []byte, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMPV6)
	if err != nil {
		return -1, nil, err
	}
	fail := func(err error) (int, []byte, error) { unix.Close(fd); return -1, nil, err }
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface); err != nil {
		return fail(err)
	}
	for _, option := range []struct{ key, value int }{{unix.IPV6_MULTICAST_IF, link.index}, {unix.IPV6_MULTICAST_HOPS, 255}, {unix.IPV6_UNICAST_HOPS, 255}, {unix.IPV6_RECVHOPLIMIT, 1}, {unix.IPV6_RECVPKTINFO, 1}} {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, option.key, option.value); err != nil {
			return fail(err)
		}
	}
	var filter unix.ICMPv6Filter
	for i := range filter.Data {
		filter.Data[i] = ^uint32(0)
	}
	filter.Data[134/32] &^= uint32(1) << uint(134%32)
	if err := unix.SetsockoptICMPv6Filter(fd, unix.IPPROTO_ICMPV6, unix.ICMPV6_FILTER, &filter); err != nil {
		return fail(err)
	}
	var group unix.IPv6Mreq
	group.Interface = uint32(link.index)
	group.Multiaddr = netip.MustParseAddr("ff02::1").As16()
	if err := unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, &group); err != nil {
		return fail(err)
	}
	return fd, unix.PktInfo6(&unix.Inet6Pktinfo{Ifindex: uint32(link.index), Addr: link.source.As16()}), nil
}

func sendSolicitation(fd int, pktinfo []byte, link linkInfo) error {
	b := []byte{133, 0, 0, 0, 0, 0, 0, 0}
	if len(link.hardware) == 6 {
		b = append(b, 1, 1)
		b = append(b, link.hardware...)
	}
	to := &unix.SockaddrInet6{ZoneId: uint32(link.index), Addr: netip.MustParseAddr("ff02::2").As16()}
	if _, err := unix.SendmsgN(fd, b, pktinfo, to, 0); err != nil {
		return fmt.Errorf("send router solicitation: %w", err)
	}
	return nil
}

func solicitationDelay() time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 500 * time.Millisecond
	}
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(time.Second+1))
}
