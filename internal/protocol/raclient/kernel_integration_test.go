//go:build linux && integration

// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// No fixture mutation or protocol socket occurs in the parent process. The
// complete exchange runs after re-exec into fresh user, network, PID and mount
// namespaces. A private procfs permits per-link sysctl fixture setup even when
// the enclosing container mounts its own /proc/sys read-only.
func TestIsolatedNativeRAClient(t *testing.T) {
	if os.Getenv("ROUTERD2_RA_CLIENT_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		parent := os.Getenv("ROUTERD2_RA_CLIENT_PARENT")
		if err != nil || parent == "" || current == parent {
			t.Fatal("refusing RA client fixture outside a fresh network namespace")
		}
		mount, err := os.Readlink("/proc/self/ns/mnt")
		parentMount := os.Getenv("ROUTERD2_RA_CLIENT_PARENT_MOUNT")
		if err != nil || parentMount == "" || mount == parentMount {
			t.Fatal("refusing RA client fixture outside a fresh mount namespace")
		}
		if os.Getpid() != 1 {
			t.Fatal("refusing to mount fixture procfs outside a fresh PID namespace")
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			t.Fatal(err)
		}
		mounts, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil || strings.Contains(string(mounts), " shared:") || strings.Contains(string(mounts), " master:") {
			t.Fatal("fixture mount namespace has non-private propagation")
		}
		if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			t.Fatal(err)
		}
		nativeRAClient(t)
		return
	}
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	parentMount, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestIsolatedNativeRAClient$", "-test.v", "-test.timeout=35s")
	cmd.Env = append(os.Environ(), "ROUTERD2_RA_CLIENT_CHILD=1", "ROUTERD2_RA_CLIENT_PARENT="+parent, "ROUTERD2_RA_CLIENT_PARENT_MOUNT="+parentMount)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNET | unix.CLONE_NEWNS | unix.CLONE_NEWPID,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		if errors.Is(err, syscall.ENOENT) {
			unavailable = exec.Command(executable, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated RA client namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated RA client fixture: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func inRANamespace(ns netns.NsHandle, fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	prior, err := netns.Get()
	if err != nil {
		return err
	}
	defer prior.Close()
	if err = netns.Set(ns); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, netns.Set(prior)) }()
	return fn()
}

func raPair(t *testing.T) (netlink.Link, int, int) {
	t.Helper()
	runtime.LockOSThread()
	prior, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	peerNS, createErr := netns.New()
	restoreErr := netns.Set(prior)
	prior.Close()
	runtime.UnlockOSThread()
	if err = errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peerNS.Close() })
	if err = netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "rawan"}, PeerName: "rarouter"}); err != nil {
		t.Fatal(err)
	}
	client, err := netlink.LinkByName("rawan")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName("rarouter")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetNsFd(peer, int(peerNS)); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatalf("peer routing netlink handle: %v", err)
	}
	t.Cleanup(h.Close)
	peer, err = h.LinkByName("rarouter")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"accept_ra", "autoconf"} {
		// These writes are exclusively fixture setup in the isolated namespace,
		// representing the single network writer. Production Run only reads them.
		if err = os.WriteFile("/proc/sys/net/ipv6/conf/rawan/"+key, []byte("0"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := netlink.ParseAddr("fe80::1/64")
	a.Flags = unix.IFA_F_NODAD
	if err = netlink.AddrAdd(client, a); err != nil {
		t.Fatal(err)
	}
	a, _ = netlink.ParseAddr("fe80::2/64")
	a.Flags = unix.IFA_F_NODAD
	if err = h.AddrAdd(peer, a); err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(client); err != nil {
		t.Fatal(err)
	}
	if err = h.LinkSetUp(peer); err != nil {
		t.Fatal(err)
	}
	fd := -1
	err = inRANamespace(peerNS, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMPV6)
		if err != nil {
			return err
		}
		if err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "rarouter"); err != nil {
			return err
		}
		for _, opt := range []struct{ key, value int }{{unix.IPV6_MULTICAST_IF, peer.Attrs().Index}, {unix.IPV6_MULTICAST_HOPS, 255}, {unix.IPV6_RECVHOPLIMIT, 1}, {unix.IPV6_RECVPKTINFO, 1}} {
			if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, opt.key, opt.value); err != nil {
				return err
			}
		}
		var filter unix.ICMPv6Filter
		for i := range filter.Data {
			filter.Data[i] = ^uint32(0)
		}
		filter.Data[133/32] &^= uint32(1) << uint(133%32)
		if err = unix.SetsockoptICMPv6Filter(fd, unix.IPPROTO_ICMPV6, unix.ICMPV6_FILTER, &filter); err != nil {
			return err
		}
		return unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, &unix.IPv6Mreq{Interface: uint32(peer.Attrs().Index), Multiaddr: netip.MustParseAddr("ff02::2").As16()})
	})
	if fd >= 0 {
		t.Cleanup(func() { unix.Close(fd) })
	}
	if err != nil {
		t.Fatal(err)
	}
	return client, fd, peer.Attrs().Index
}

func readSolicitation(t *testing.T, fd int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(p, 100); err != nil {
			if err == unix.EINTR {
				continue
			}
			t.Fatal(err)
		}
		if p[0].Revents&unix.POLLIN == 0 {
			continue
		}
		b, oob := make([]byte, 2048), make([]byte, 256)
		n, cn, _, from, err := unix.Recvmsg(fd, b, oob, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		b = b[:n]
		if n < 8 || b[0] != 133 {
			continue
		}
		source, ok := from.(*unix.SockaddrInet6)
		if !ok || netip.AddrFrom16(source.Addr) != netip.MustParseAddr("fe80::1") {
			t.Fatal("RS did not use configured link-local source")
		}
		cms, err := unix.ParseSocketControlMessage(oob[:cn])
		if err != nil {
			t.Fatal(err)
		}
		hop := 0
		for _, cm := range cms {
			if cm.Header.Level == unix.IPPROTO_IPV6 && cm.Header.Type == unix.IPV6_HOPLIMIT && len(cm.Data) >= 4 {
				hop = int(binary.NativeEndian.Uint32(cm.Data))
			}
		}
		if hop != 255 || checksum(b, netip.MustParseAddr("fe80::1"), netip.MustParseAddr("ff02::2")) != 0 || len(b) != 16 || b[8] != 1 || b[9] != 1 {
			t.Fatalf("invalid native RS header, checksum, hop limit or Ethernet LLA: hop=%d bytes=%x", hop, b)
		}
		return
	}
	t.Fatal("native client did not emit a router solicitation")
}

func sendAdvertisement(t *testing.T, fd, index, hop int, packet packet) {
	t.Helper()
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, hop); err != nil {
		t.Fatal(err)
	}
	info := unix.PktInfo6(&unix.Inet6Pktinfo{Ifindex: uint32(index), Addr: netip.MustParseAddr("fe80::2").As16()})
	to := &unix.SockaddrInet6{ZoneId: uint32(index), Addr: netip.MustParseAddr("ff02::1").As16()}
	if _, err := unix.SendmsgN(fd, packet.payload, info, to, 0); err != nil {
		t.Fatal(err)
	}
}

func nativeRAClient(t *testing.T) {
	client, fd, index := raPair(t)
	cfg := Config{Resource: "test-ra", Interface: "rawan", StatePath: filepath.Join(t.TempDir(), "ra.json"), SourceAddress: "fe80::1", InterfaceID: "::1234", AllowedRouters: []string{"fe80::2"}}
	start := func() (<-chan Snapshot, <-chan error, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		updates := make(chan Snapshot, 64)
		done, ready := make(chan error, 1), make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			done <- Run(ctx, cfg, EffectsFunc(func(ctx context.Context, s Snapshot) error {
				select {
				case updates <- s:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}), func() { close(ready) })
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("RA fixture cleanup could not stop its worker")
			}
		})
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("RA client failed before readiness: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("RA client readiness timeout")
		}
		return updates, done, cancel
	}
	wait := func(updates <-chan Snapshot, done <-chan error, predicate func(Snapshot) bool) Snapshot {
		t.Helper()
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		for {
			select {
			case s := <-updates:
				if err := ValidateSnapshot(cfg, s); err != nil {
					t.Fatalf("invalid published snapshot: %v", err)
				}
				if predicate(s) {
					return s
				}
			case err := <-done:
				t.Fatalf("RA client exited: %v", err)
			case <-timer.C:
				t.Fatal("RA snapshot timeout")
			}
		}
	}
	stop := func(updates <-chan Snapshot, done <-chan error, cancel context.CancelFunc) {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected RA client exit: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RA cancellation did not stop raw socket loop")
		}
		for {
			select {
			case s := <-updates:
				if !s.Active {
					return
				}
			default:
				t.Fatal("RA cancellation did not withdraw its full effect snapshot")
			}
		}
	}
	updates, done, cancel := start()
	initial := wait(updates, done, func(s Snapshot) bool { return s.Active })
	if initial.Received != 0 || len(initial.Routes)+len(initial.Addresses) != 0 {
		t.Fatal("ready claimed unobserved addresses or routes")
	}
	readSolicitation(t, fd)
	sendAdvertisement(t, fd, index, 254, advert("fe80::2", 0, 0, pio("2001:db8:bad::/64", 0xc0, 50, 30)))
	rejected := wait(updates, done, func(s Snapshot) bool { return s.Rejected > 0 })
	if rejected.Received != 0 || len(rejected.Addresses)+len(rejected.Routes) != 0 {
		t.Fatal("off-link hop limit altered protocol state")
	}
	sendAdvertisement(t, fd, index, 255, advert("fe80::2", 4, 8, pio("2001:db8:1::/64", 0xc0, 8, 2), rio("2001:db8:2::/48", 0, 6), rdnss(3, "fe80::53"), dnssl(7)))
	learned := wait(updates, done, func(s Snapshot) bool { return s.Received == 1 })
	if len(learned.Addresses) != 1 || learned.Addresses[0].Address != "2001:db8:1::1234/128" || len(learned.Prefixes) != 1 || len(learned.Routes) != 2 || len(learned.DNSServers) != 1 || len(learned.SearchDomains) != 1 {
		t.Fatalf("incomplete native RA learning: %+v", learned)
	}
	addresses, err := netlink.AddrList(client, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if !address.IP.IsLinkLocalUnicast() {
			t.Fatal("protocol worker or competing kernel autoconfiguration wrote an address")
		}
	}
	// A rejected packet changes status only. Publishing that status must not
	// perturb UTC lifetimes and cause an unnecessary network reconfiguration.
	sendAdvertisement(t, fd, index, 254, advert("fe80::2", 0, 0))
	statusOnly := wait(updates, done, func(s Snapshot) bool { return s.Rejected > learned.Rejected })
	if statusOnly.Received != learned.Received || len(statusOnly.Addresses) != 1 || !statusOnly.Addresses[0].ValidUntil.Equal(learned.Addresses[0].ValidUntil) || !statusOnly.Addresses[0].PreferredUntil.Equal(learned.Addresses[0].PreferredUntil) {
		t.Fatal("native status-only publication shifted an absolute SLAAC deadline")
	}
	stop(updates, done, cancel)
	updates, done, cancel = start()
	restored := wait(updates, done, func(s Snapshot) bool { return s.Active && s.Received == 1 })
	if !restored.Addresses[0].ValidUntil.Equal(learned.Addresses[0].ValidUntil) || !restored.Addresses[0].PreferredUntil.Equal(learned.Addresses[0].PreferredUntil) {
		t.Fatal("same-boot native restart reset or shifted SLAAC lifetime")
	}
	sendAdvertisement(t, fd, index, 255, advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, 0, 0), rdnss(0, "fe80::53")))
	withdrawn := wait(updates, done, func(s Snapshot) bool { return s.Received == 2 })
	if len(withdrawn.Addresses) != 1 || !withdrawn.Addresses[0].Deprecated || len(withdrawn.Prefixes) != 0 || len(withdrawn.Routes) != 1 || withdrawn.Routes[0].Destination != "2001:db8:2::/48" || len(withdrawn.DNSServers) != 0 || len(withdrawn.SearchDomains) != 1 {
		t.Fatalf("zero-lifetime native withdrawal violated independent lifetimes: %+v", withdrawn)
	}
	wait(updates, done, func(s Snapshot) bool {
		return s.Active && s.Received == 2 && len(s.Addresses)+len(s.Prefixes)+len(s.Routes)+len(s.DNSServers)+len(s.SearchDomains) == 0
	})
	stop(updates, done, cancel)
	calledReady := false
	err = Run(context.Background(), cfg, EffectsFunc(func(context.Context, Snapshot) error { return errors.New("injected single-writer failure") }), func() { calledReady = true })
	if err == nil || !strings.Contains(err.Error(), "injected single-writer failure") || calledReady {
		t.Fatal("effect failure was swallowed or readiness was announced")
	}
	if err = os.WriteFile("/proc/sys/net/ipv6/conf/rawan/accept_ra", []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), cfg, EffectsFunc(func(context.Context, Snapshot) error { t.Fatal("published without sysctl ownership"); return nil }), nil)
	if err == nil || !strings.Contains(err.Error(), "accept_ra=0") {
		t.Fatal("competing kernel RA ownership was accepted")
	}
}
