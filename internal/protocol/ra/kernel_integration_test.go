//go:build linux && integration

package ra

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The parent does not create sockets, run a protocol worker, or modify links.
// Every mutation happens after re-exec has proved fresh user AND net namespaces.
func TestIsolatedLearnedDNSLifetimes(t *testing.T) {
	if os.Getenv("ROUTERD2_RA_DNS_CHILD") == "1" {
		for _, kind := range []string{"net", "user"} {
			current, err := os.Readlink("/proc/self/ns/" + kind)
			parent := os.Getenv("ROUTERD2_RA_DNS_PARENT_" + kind)
			if err != nil || parent == "" || current == parent {
				t.Fatalf("refusing RA DNS fixture outside fresh %s namespace", kind)
			}
		}
		nativeLearnedDNS(t)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestIsolatedLearnedDNSLifetimes$", "-test.v", "-test.timeout=35s")
	command.Env = append(os.Environ(), "ROUTERD2_RA_DNS_CHILD=1")
	for _, kind := range []string{"net", "user"} {
		identity, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		command.Env = append(command.Env, "ROUTERD2_RA_DNS_PARENT_"+kind+"="+identity)
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false,
	}
	output, err := command.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		if errors.Is(err, syscall.ENOENT) {
			unavailable = exec.Command(executable, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated user/network namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated RA learned DNS fixture: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func dnsPeer(t *testing.T) (int, int) {
	t.Helper()
	runtime.LockOSThread()
	current, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	peer, createErr := netns.New()
	restoreErr := netns.Set(current)
	current.Close()
	runtime.UnlockOSThread()
	if err := errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "ra-dns0"}, PeerName: "ra-dns-peer"}); err != nil {
		t.Fatal(err)
	}
	local, err := netlink.LinkByName("ra-dns0")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := netlink.LinkByName("ra-dns-peer")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(remote, int(peer)); err != nil {
		t.Fatal(err)
	}
	handle, err := netlink.NewHandleAt(peer, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal("open peer route netlink socket:", err)
	}
	defer handle.Close()
	remote, err = handle.LinkByName("ra-dns-peer")
	if err != nil {
		t.Fatal(err)
	}
	localAddress, _ := netlink.ParseAddr("fe80::1/64")
	localAddress.Flags = unix.IFA_F_NODAD
	peerAddress, _ := netlink.ParseAddr("fe80::2/64")
	peerAddress.Flags = unix.IFA_F_NODAD
	for _, err := range []error{netlink.AddrAdd(local, localAddress), handle.AddrAdd(remote, peerAddress), netlink.LinkSetUp(local), handle.LinkSetUp(remote)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	fd := -1
	// The raw capture socket is created inside the peer namespace and retained
	// after the thread returns to the isolated router namespace.
	runtime.LockOSThread()
	current, err = netns.Get()
	if err == nil {
		err = netns.Set(peer)
	}
	if err == nil {
		fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
		if err != nil {
			err = fmt.Errorf("open peer ICMPv6 socket: %w", err)
		}
	}
	if err == nil {
		err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "ra-dns-peer")
	}
	for _, option := range []int{unix.IPV6_RECVHOPLIMIT, unix.IPV6_RECVPKTINFO} {
		if err == nil {
			err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, option, 1)
		}
	}
	if current.IsOpen() {
		err = errors.Join(err, netns.Set(current))
		current.Close()
	}
	runtime.UnlockOSThread()
	if fd >= 0 {
		t.Cleanup(func() { unix.Close(fd) })
	}
	if err != nil {
		t.Fatal(err)
	}
	return fd, remote.Attrs().Index
}

func nativeLearnedDNS(t *testing.T) {
	fd, peerIndex := dnsPeer(t)
	now := time.Now().UTC()
	cfg := example()
	cfg.Interface, cfg.SourceAddress = "ra-dns0", "fe80::1"
	cfg.Prefixes = nil
	cfg.LearnedRDNSS = []DNSServer{
		{Address: "2001:db8:1::1", ValidUntil: now.Add(time.Second)},
		{Address: "2001:db8::53", ValidUntil: now.Add(8 * time.Second)},
		{Address: "2001:db8::54", ValidUntil: now.Add(time.Minute)},
	}
	cfg.LearnedDNSSL = []SearchDomain{
		{Domain: "lan.example", ValidUntil: now.Add(time.Second)},
		{Domain: "learned.example", ValidUntil: now.Add(8 * time.Second)},
		{Domain: "remove.example", ValidUntil: now.Add(time.Minute)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	updates := make(chan Config, 1)
	statuses := make(chan Snapshot, 64)
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- RunWithUpdates(ctx, cfg, updates, EffectsFunc(func(_ context.Context, s Snapshot) error { statuses <- s; return nil }), func() { close(ready) })
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("RA worker failed to stop")
			}
		}
	})
	select {
	case <-ready:
	case err := <-done:
		stopped = true
		t.Fatal("RA worker exited before ready", err)
	case <-time.After(4 * time.Second):
		t.Fatal("RA worker did not become ready")
	}
	read := func(withdraw bool) (map[string]uint32, map[string]uint32) {
		t.Helper()
		payload, ancillary := make([]byte, 2048), make([]byte, 256)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, 100); err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				t.Fatal(err)
			}
			if poll[0].Revents&unix.POLLIN == 0 {
				continue
			}
			n, control, flags, source, err := unix.Recvmsg(fd, payload, ancillary, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n < 16 || payload[0] != 134 {
				continue
			}
			if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
				t.Fatal("truncated RA capture")
			}
			from, ok := source.(*unix.SockaddrInet6)
			if !ok || netip.AddrFrom16(from.Addr).String() != "fe80::1" {
				t.Fatal("unexpected RA source", source)
			}
			messages, err := unix.ParseSocketControlMessage(ancillary[:control])
			if err != nil {
				t.Fatal(err)
			}
			hop, index := 0, 0
			for _, m := range messages {
				if m.Header.Level != unix.IPPROTO_IPV6 {
					continue
				}
				if m.Header.Type == unix.IPV6_HOPLIMIT && len(m.Data) >= 4 {
					hop = int(binary.NativeEndian.Uint32(m.Data))
				}
				if m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= 20 {
					index = int(binary.NativeEndian.Uint32(m.Data[16:]))
				}
			}
			if hop != 255 || index != peerIndex {
				t.Fatalf("RA received on interface %d hop %d", index, hop)
			}
			if (binary.BigEndian.Uint16(payload[6:]) == 0) != withdraw {
				t.Fatal("DNS update unexpectedly changed router availability")
			}
			return dnsLifetimes(t, payload[:n])
		}
		t.Fatal("RA not received")
		return nil, nil
	}
	static := func(servers, domains map[string]uint32) {
		t.Helper()
		if servers["2001:db8:1::1"] != 20 || domains["lan.example"] != 20 {
			t.Fatal("expired learned duplicate withdrew static DNS", servers, domains)
		}
	}
	servers, domains := read(false)
	static(servers, domains)
	first := servers["2001:db8::53"]
	if first == 0 || first > 8 || domains["learned.example"] != first {
		t.Fatal("initial learned DNS lifetime wrong", servers, domains)
	}
	servers, domains = read(false)
	static(servers, domains)
	if servers["2001:db8::53"] == 0 || servers["2001:db8::53"] >= first || domains["learned.example"] != servers["2001:db8::53"] {
		t.Fatal("unchanged learned DNS did not count down", first, servers, domains)
	}
	renewed := normalized(cfg)
	renewed.LearnedRDNSS = []DNSServer{{Address: "2001:db8::53", ValidUntil: now.Add(11 * time.Second)}}
	renewed.LearnedDNSSL = []SearchDomain{{Domain: "learned.example", ValidUntil: now.Add(11 * time.Second)}}
	updates <- renewed
	close(updates) // No controller input is available to remove expired DNS.
	servers, domains = read(false)
	static(servers, domains)
	if value, present := servers["2001:db8::54"]; !present || value != 0 {
		t.Fatal("removed learned server did not withdraw", servers)
	}
	if value, present := domains["remove.example"]; !present || value != 0 {
		t.Fatal("removed learned search domain did not withdraw", domains)
	}
	if servers["2001:db8::53"] == 0 || domains["learned.example"] == 0 {
		t.Fatal("renewal withdrew a retained learned option", servers, domains)
	}
	last := servers["2001:db8::53"]
	for {
		servers, domains = read(false)
		static(servers, domains)
		value, present := servers["2001:db8::53"]
		if !present || value > last || domains["learned.example"] != value {
			t.Fatal("closed update stream extended or lost the learned deadline", last, servers, domains)
		}
		if value == 0 {
			break
		}
		last = value
	}
	cancel()
	select {
	case err := <-done:
		stopped = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal("RA shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RA worker did not join on cancellation")
	}
	servers, domains = read(true)
	for _, values := range []map[string]uint32{servers, domains} {
		for name, value := range values {
			if value != 0 {
				t.Fatal("shutdown did not withdraw DNS", name, value)
			}
		}
	}
	var lastStatus Snapshot
	for len(statuses) != 0 {
		lastStatus = <-statuses
	}
	if lastStatus.Generation != 2 || lastStatus.RejectedUpdates != 0 || !lastStatus.Withdrawing {
		t.Fatal("unexpected worker lifecycle", lastStatus)
	}
}
