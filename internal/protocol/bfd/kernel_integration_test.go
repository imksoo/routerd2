//go:build linux && integration

// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The same integration binary can boot as /init in a disposable no-NIC QEMU
// guest. Ordinary execution never mounts filesystems or loads kernel modules.
func TestMain(m *testing.M) {
	if os.Getpid() != 1 {
		os.Exit(m.Run())
	}
	if err := boot.Bootstrap(boot.Options{Platform: "appliance", RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd"}); err != nil {
		fmt.Println("BFD_QEMU_TEST_FAIL bootstrap:", err)
		powerOffBFDGuest()
	}
	if err := kmod.Load([]string{"veth"}); err != nil {
		fmt.Println("BFD_QEMU_TEST_FAIL modules:", err)
		powerOffBFDGuest()
	}
	_ = os.Setenv("ROUTERD2_INTEGRATION_REQUIRED", "1")
	if code := m.Run(); code == 0 {
		fmt.Println("BFD_QEMU_TEST_PASS")
	} else {
		fmt.Println("BFD_QEMU_TEST_FAIL exit:", code)
	}
	powerOffBFDGuest()
}

func powerOffBFDGuest() { unix.Sync(); _ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); select {} }

// Parent is read-only: every link/address mutation follows proof that the child
// has a different, freshly created network namespace, even inside a VM.
func TestIsolatedNativeBFD(t *testing.T) {
	if os.Getenv("ROUTERD2_BFD_NETNS_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		parent := os.Getenv("ROUTERD2_BFD_PARENT_NETNS")
		if err != nil || parent == "" || current == parent {
			t.Fatal("refusing BFD fixture mutation outside a verified disposable namespace")
		}
		for _, family := range []string{"ipv4", "ipv6"} {
			t.Run(family, func(t *testing.T) { independentPeerFixture(t, family) })
		}
		return
	}
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestIsolatedNativeBFD$", "-test.v", "-test.timeout=35s")
	cmd.Env = append(os.Environ(), "ROUTERD2_BFD_NETNS_CHILD=1", "ROUTERD2_BFD_PARENT_NETNS="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	output, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
		if errors.Is(err, unix.ENOENT) {
			unavailable = exec.Command(exe, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated BFD network namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated BFD test failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func newBFDPeerNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	peer, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(current); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

func inBFDNamespace(namespace netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current, err := netns.Get()
	if err != nil {
		return err
	}
	defer current.Close()
	if err := netns.Set(namespace); err != nil {
		return err
	}
	err = fn()
	return errors.Join(err, netns.Set(current))
}

type statusProbe struct {
	mu   sync.Mutex
	last Snapshot
}

func (p *statusProbe) update(cfg Config) Effects {
	return EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ValidateSnapshot(cfg, s); err != nil {
			return err
		}
		p.mu.Lock()
		p.last = s
		p.mu.Unlock()
		return nil
	})
}
func (p *statusProbe) snapshot() Snapshot { p.mu.Lock(); defer p.mu.Unlock(); return p.last }

// This fixture encodes/decodes RFC control fields independently; it deliberately
// does not use control.bytes, parseControl, session or nativeTransport.
type peerObservation struct {
	state, flags     byte
	my, your, tx, rx uint32
	port             int
}
type independentPeer struct {
	receive, send, alias, lowPort *net.UDPConn
	destination                   *net.UDPAddr
	ipv6                          bool
}

func peerVector(state, flags byte, my, your, tx uint32) []byte {
	b := make([]byte, 24)
	b[0], b[1], b[2], b[3] = 0x20, state<<6|flags, 3, 24
	binary.BigEndian.PutUint32(b[4:8], my)
	binary.BigEndian.PutUint32(b[8:12], your)
	binary.BigEndian.PutUint32(b[12:16], tx)
	binary.BigEndian.PutUint32(b[16:20], 50000)
	return b
}

func setPeerTTL(conn *net.UDPConn, ipv6 bool, ttl int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		level, option := unix.IPPROTO_IP, unix.IP_TTL
		if ipv6 {
			level, option = unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS
		}
		optionErr = unix.SetsockoptInt(int(fd), level, option, ttl)
	})
	return errors.Join(err, optionErr)
}

func (p *independentPeer) transmit(conn *net.UDPConn, b []byte, ttl int) error {
	if err := setPeerTTL(conn, p.ipv6, ttl); err != nil {
		return err
	}
	_, err := conn.WriteToUDP(b, p.destination)
	return err
}

func (p *independentPeer) read(timeout time.Duration) (peerObservation, error) {
	_ = p.receive.SetReadDeadline(time.Now().Add(timeout))
	b, oob := make([]byte, 512), make([]byte, 256)
	n, on, flags, from, err := p.receive.ReadMsgUDP(b, oob)
	if err != nil {
		return peerObservation{}, err
	}
	if n != 24 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || b[0]>>5 != 1 || b[3] != 24 || b[2] != 3 || b[1]&0x0f != 0 || from.Port < 49152 || !from.IP.Equal(p.destination.IP) || binary.BigEndian.Uint32(b[20:24]) != 0 {
		return peerObservation{}, fmt.Errorf("invalid outbound BFD envelope: %x source=%v", b[:n], from)
	}
	controls, err := unix.ParseSocketControlMessage(oob[:on])
	if err != nil {
		return peerObservation{}, err
	}
	ttl := 0
	for _, c := range controls {
		if c.Header.Level == unix.IPPROTO_IP && c.Header.Type == unix.IP_TTL || c.Header.Level == unix.IPPROTO_IPV6 && c.Header.Type == unix.IPV6_HOPLIMIT {
			if len(c.Data) == 4 {
				ttl = int(binary.NativeEndian.Uint32(c.Data))
			}
		}
	}
	if ttl != 255 {
		return peerObservation{}, fmt.Errorf("outbound BFD hop limit=%d", ttl)
	}
	return peerObservation{state: b[1] >> 6, flags: b[1] & 0x3f, my: binary.BigEndian.Uint32(b[4:8]), your: binary.BigEndian.Uint32(b[8:12]), tx: binary.BigEndian.Uint32(b[12:16]), rx: binary.BigEndian.Uint32(b[16:20]), port: from.Port}, nil
}

func (p *independentPeer) wait(t *testing.T, match func(peerObservation) bool) peerObservation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		observation, err := p.read(100 * time.Millisecond)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		if match(observation) {
			return observation
		}
	}
	t.Fatal("independent wire peer did not observe expected BFD control")
	return peerObservation{}
}

func independentPeerFixture(t *testing.T, family string) {
	t.Helper()
	namespace := newBFDPeerNamespace(t)
	h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "bfd0"}, PeerName: "bfdpeer0"}
	if err := h.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err == nil {
			defer cleanup.Close()
			if link, err := cleanup.LinkByName("bfd0"); err == nil {
				_ = cleanup.LinkDel(link)
			}
		}
	})
	other, err := h.LinkByName("bfdpeer0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetNsFd(other, int(namespace)); err != nil {
		t.Fatal(err)
	}
	local, err := h.LinkByName("bfd0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(local); err != nil {
		t.Fatal(err)
	}
	source, remote, alias, suffix, network := "192.0.2.1", "192.0.2.2", "192.0.2.3", "/24", "udp4"
	if family == "ipv6" {
		source, remote, alias, suffix, network = "2001:db8:bf::1", "2001:db8:bf::2", "2001:db8:bf::3", "/64", "udp6"
	}
	address, err := netlink.ParseAddr(source + suffix)
	if err != nil {
		t.Fatal(err)
	}
	address.Flags = unix.IFA_F_NODAD
	if err := h.AddrAdd(local, address); err != nil {
		t.Fatal(err)
	}
	peer := &independentPeer{destination: &net.UDPAddr{IP: net.ParseIP(source), Port: 3784}, ipv6: family == "ipv6"}
	if err := inBFDNamespace(namespace, func() error {
		h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return err
		}
		defer h.Close()
		link, err := h.LinkByName("bfdpeer0")
		if err != nil {
			return err
		}
		if err := h.LinkSetUp(link); err != nil {
			return err
		}
		for _, ip := range []string{remote, alias} {
			a, err := netlink.ParseAddr(ip + suffix)
			if err != nil {
				return err
			}
			a.Flags = unix.IFA_F_NODAD
			if err := h.AddrAdd(link, a); err != nil {
				return err
			}
		}
		for _, item := range []struct {
			target **net.UDPConn
			ip     string
			port   int
		}{{&peer.receive, remote, 3784}, {&peer.send, remote, 49170}, {&peer.alias, alias, 49171}, {&peer.lowPort, remote, 3783}} {
			*item.target, err = net.ListenUDP(network, &net.UDPAddr{IP: net.ParseIP(item.ip), Port: item.port})
			if err != nil {
				return err
			}
		}
		raw, err := peer.receive.SyscallConn()
		if err != nil {
			return err
		}
		var optionErr error
		err = raw.Control(func(fd uintptr) {
			level, option := unix.IPPROTO_IP, unix.IP_RECVTTL
			if peer.ipv6 {
				level, option = unix.IPPROTO_IPV6, unix.IPV6_RECVHOPLIMIT
			}
			optionErr = unix.SetsockoptInt(int(fd), level, option, 1)
		})
		return errors.Join(err, optionErr)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.receive.Close(); peer.send.Close(); peer.alias.Close(); peer.lowPort.Close() })
	cfg := Normalize(Config{Resource: "native-" + family, Interface: "bfd0", SourceAddress: source, Peers: []Peer{{Address: remote, MinTxMilliseconds: 50, MinRxMilliseconds: 50, DetectMultiplier: 3}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe, ready, done := &statusProbe{}, make(chan struct{}), make(chan error, 1)
	go func() { done <- Run(ctx, cfg, probe.update(cfg), func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("native worker startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("native BFD did not become ready")
	}
	first := peer.wait(t, func(p peerObservation) bool { return p.state == 1 && p.your == 0 })
	if first.my == 0 || first.tx < 1000000 || first.rx != 50000 {
		t.Fatalf("invalid initial native control: %+v", first)
	}
	clock, err := ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := openTransport(cfg, clock); err == nil {
		duplicate.Close()
		t.Fatal("second owner claimed the same interface/family")
	}
	multihop := cfg
	multihop.Multihop, multihop.Interface = true, ""
	transportProbe, err := openTransport(multihop, clock)
	if err != nil {
		t.Fatal("open multihop receive endpoint", err)
	}
	defer func() {
		if err := transportProbe.Close(); err != nil {
			t.Error(err)
		}
	}()
	duplicate, claimErr := openTransport(multihop, clock)
	if claimErr == nil {
		duplicate.Close()
		t.Fatal("address reuse admitted two multihop receivers in one domain")
	}
	if !errors.Is(claimErr, unix.EADDRINUSE) {
		t.Fatal("duplicate multihop endpoint failed without an ownership conflict", claimErr)
	}
	remoteDisc := uint32(0x8675309)
	handshake := func(from *net.UDPConn, disc uint32) {
		t.Helper()
		if err := peer.transmit(from, peerVector(1, 0, disc, 0, 1000000), 255); err != nil {
			t.Fatal(err)
		}
		peer.wait(t, func(p peerObservation) bool { return p.state == 2 && p.your == disc })
		if err := peer.transmit(from, peerVector(2, 0, disc, first.my, 1000000), 255); err != nil {
			t.Fatal(err)
		}
		up := peer.wait(t, func(p peerObservation) bool { return p.state == 3 && p.flags&0x20 != 0 && p.your == disc })
		if up.port != first.port || up.my != first.my || up.tx != 50000 {
			t.Fatalf("session source/discriminator/active interval changed: %+v", up)
		}
		if err := peer.transmit(from, peerVector(3, 0x10, disc, first.my, 50000), 255); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, func() bool {
			s := probe.snapshot()
			return len(s.Peers) == 1 && s.Peers[0].State == StateUp && !s.Peers[0].Polling
		}, "native Poll/Final did not complete")
	}
	handshake(peer.send, remoteDisc)
	if err := peer.transmit(peer.send, peerVector(3, 0x20, remoteDisc, first.my, 50000), 255); err != nil {
		t.Fatal(err)
	}
	final := peer.wait(t, func(p peerObservation) bool { return p.flags&0x10 != 0 })
	if final.flags&0x20 != 0 || final.port != first.port {
		t.Fatal("Final was combined with Poll or source port changed")
	}
	// Wait for a snapshot containing the last legitimate Poll receipt; all the
	// traffic below is invalid and must not renew that grant.
	baselineCount := probe.snapshot().Peers[0].Received
	if err := peer.transmit(peer.send, peerVector(3, 0, remoteDisc, first.my, 50000), 255); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return probe.snapshot().Peers[0].Received > baselineCount }, "last valid receive was not published")
	baseline := probe.snapshot().Peers[0].LastReceivedBootNanoseconds
	for i := 0; i < 16; i++ {
		valid := peerVector(3, 0, remoteDisc, first.my, 50000)
		conn, ttl := peer.send, 255
		switch i % 5 {
		case 0:
			ttl = 254
		case 1:
			conn = peer.alias
		case 2:
			conn = peer.lowPort
		case 3:
			binary.BigEndian.PutUint32(valid[8:12], first.my+1)
		case 4:
			valid[1] |= 4
		}
		if err := peer.transmit(conn, valid, ttl); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	waitUntil(t, func() bool { s := probe.snapshot(); return s.Peers[0].State == StateDown && s.Rejected >= 10 }, "invalid traffic prevented detection timeout")
	s := probe.snapshot()
	if s.Peers[0].LastReceivedBootNanoseconds != baseline || s.Peers[0].RemoteDiscriminator != 0 || s.Peers[0].DownTransitions != 1 {
		t.Fatalf("invalid traffic renewed native health or lost expiry history: %+v", s)
	}
	handshake(peer.send, remoteDisc+1)
	if s := probe.snapshot(); s.Peers[0].UpTransitions != 2 || s.Peers[0].DownTransitions != 1 {
		t.Fatal("peer restart did not retain loss/recovery history")
	}
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	s = probe.snapshot()
	if s.Active || s.Peers[0].State != StateAdminDown || s.Peers[0].DeadlineBootNanoseconds != 0 {
		t.Fatal("native cancellation did not publish final withdrawal")
	}
	peer.wait(t, func(p peerObservation) bool { return p.state == 0 })
	// New ownership succeeds after the old worker has joined and released all
	// claims. P2P then accepts a different valid source while TX stays at remote.
	cfg.PointToPoint = true
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	probe, ready, done = &statusProbe{}, make(chan struct{}), make(chan error, 1)
	go func() { done <- Run(ctx2, cfg, probe.update(cfg), func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("native restart: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("native restart did not become ready")
	}
	first = peer.wait(t, func(p peerObservation) bool { return p.state == 1 && p.your == 0 })
	handshake(peer.alias, remoteDisc+2)
	if got := probe.snapshot().Peers[0].ObservedSource; got != alias {
		t.Fatalf("point-to-point source alias was not recorded: %s", got)
	}
	// Only the configured destination has a receiver. Seeing the exchanged Up
	// above therefore proves a source change did not retarget outbound packets.
	if err := h.AddrDel(local, address); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err == nil {
		t.Fatal("loss of owned local source did not stop the native worker")
	}
	if probe.snapshot().Active {
		t.Fatal("source loss did not withdraw native worker")
	}
	if _, err := h.LinkByName("bfd0"); err != nil {
		t.Fatal("BFD removed an externally owned interface")
	}
}
