//go:build linux && integration

package vrrp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// A test binary can also be used as /init in an isolated QEMU guest. This
// supports the exact native fixture when the build host disallows namespaces.
// Normal test execution never mounts filesystems or loads kernel modules.
func TestMain(m *testing.M) {
	if os.Getpid() != 1 {
		os.Exit(m.Run())
	}
	if err := boot.Bootstrap(boot.Options{Platform: "appliance", RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd"}); err != nil {
		fmt.Println("VRRP_QEMU_TEST_FAIL bootstrap:", err)
		powerOffTestGuest()
	}
	if err := kmod.Load([]string{"veth", "bridge", "macvlan"}); err != nil {
		fmt.Println("VRRP_QEMU_TEST_FAIL modules:", err)
		powerOffTestGuest()
	}
	_ = os.Setenv("ROUTERD2_INTEGRATION_REQUIRED", "1")
	code := m.Run()
	if code == 0 {
		fmt.Println("VRRP_QEMU_TEST_PASS")
	} else {
		fmt.Println("VRRP_QEMU_TEST_FAIL exit:", code)
	}
	powerOffTestGuest()
}

func powerOffTestGuest() {
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}

// The parent only forks a disposable namespace. All link/address changes happen
// after proving that the child has a different network namespace from its parent.
func TestIsolatedNativeVRRP(t *testing.T) {
	if os.Getenv("ROUTERD2_VRRP_NETNS_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		if err != nil || current == os.Getenv("ROUTERD2_VRRP_PARENT_NETNS") || os.Getenv("ROUTERD2_VRRP_PARENT_NETNS") == "" {
			t.Fatal("refusing network mutation without a distinct disposable namespace")
		}
		for _, family := range []string{"ipv4", "ipv6"} {
			t.Run(family, func(t *testing.T) { nativeFixture(t, family) })
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
	cmd := exec.Command(exe, "-test.run=^TestIsolatedNativeVRRP$", "-test.v", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), "ROUTERD2_VRRP_NETNS_CHILD=1", "ROUTERD2_VRRP_PARENT_NETNS="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	output, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
		if errors.Is(err, unix.ENOENT) {
			unavailable = exec.Command(exe, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated VRRP network namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated VRRP test failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func peerNS(t *testing.T) netns.NsHandle {
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

func insideNS(namespace netns.NsHandle, fn func() error) error {
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

type peerEffects struct {
	handle *netlink.Handle
	parent int
	owned  bool
	mu     sync.Mutex
	last   Snapshot
}

func (p *peerEffects) Apply(ctx context.Context, snapshot Snapshot) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if snapshot.Active && !p.owned {
		mac, err := net.ParseMAC(snapshot.VirtualMAC)
		if err != nil {
			return Decision{}, err
		}
		link := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: snapshot.VIPInterface, ParentIndex: p.parent, HardwareAddr: mac}, Mode: netlink.MACVLAN_MODE_BRIDGE}
		if err := p.handle.LinkAdd(link); err != nil {
			return Decision{}, err
		}
		p.owned = true
		if err := p.handle.LinkSetIP6AddrGenMode(link, 1); err != nil {
			return Decision{}, err
		}
		if err := p.handle.LinkSetUp(link); err != nil {
			return Decision{}, err
		}
		for _, cidr := range snapshot.Addresses {
			address, err := netlink.ParseAddr(cidr)
			if err != nil {
				return Decision{}, err
			}
			address.Flags = unix.IFA_F_NODAD
			if err := p.handle.AddrAdd(link, address); err != nil {
				return Decision{}, err
			}
		}
	} else if !snapshot.Active && p.owned {
		link, err := p.handle.LinkByName(snapshot.VIPInterface)
		if err != nil {
			return Decision{}, err
		}
		if err := p.handle.LinkDel(link); err != nil {
			return Decision{}, err
		}
		p.owned = false
	}
	p.last = snapshot
	return Decision{Active: snapshot.Active, Track: snapshot.Track}, nil
}

func (p *peerEffects) state() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

type failingTransport struct {
	*nativeTransport
	fail atomic.Bool
}

func (f *failingTransport) SendFrame(ctx context.Context, frame []byte) error {
	if f.fail.Load() {
		return unix.ENETDOWN
	}
	return f.nativeTransport.SendFrame(ctx, frame)
}

type nativePeer struct {
	device   *failingTransport
	effects  *peerEffects
	config   Config
	source   string
	cancel   context.CancelFunc
	done     chan error
	finished chan struct{}
}

func makeNativePeer(t *testing.T, bridge netlink.Link, family string, priority, number int) *nativePeer {
	t.Helper()
	namespace := peerNS(t)
	uplink, peerName := fmt.Sprintf("up%d", number), fmt.Sprintf("peer%d", number)
	pair := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: uplink}, PeerName: peerName}
	if err := netlink.LinkAdd(pair); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(uplink); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(peer, int(namespace)); err != nil {
		t.Fatal(err)
	}
	up, err := netlink.LinkByName(uplink)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMaster(up, bridge); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(up); err != nil {
		t.Fatal(err)
	}
	var transport *nativeTransport
	var effects *peerEffects
	c := config4()
	c.Interface, c.Resource, c.Priority = peerName, fmt.Sprintf("native-%s-%d", family, number), priority
	c.SourceAddress = fmt.Sprintf("192.0.2.%d", number+10)
	sourceCIDR := c.SourceAddress + "/24"
	if family == "ipv6" {
		c.Family, c.VirtualRouterID = "ipv6", 71
		c.Addresses = []string{"fe80::1/64", "2001:db8:1::1/64"}
		c.SourceAddress = fmt.Sprintf("fe80::%d", number+10)
		sourceCIDR = c.SourceAddress + "/64"
	}
	c.VIPInterface = ""
	c.AdvertIntervalCentiseconds = 50
	c = Normalize(c)
	if err := insideNS(namespace, func() error {
		handle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return err
		}
		t.Cleanup(handle.Close)
		link, err := handle.LinkByName(peerName)
		if err != nil {
			return err
		}
		if err := handle.LinkSetUp(link); err != nil {
			return err
		}
		address, _ := netlink.ParseAddr(sourceCIDR)
		address.Flags = unix.IFA_F_NODAD
		if err := handle.AddrAdd(link, address); err != nil {
			return err
		}
		effects = &peerEffects{handle: handle, parent: link.Attrs().Index}
		transport, _, err = openTransport(t.Context(), c)
		if err != nil {
			return err
		}
		duplicate, _, duplicateErr := openTransport(t.Context(), c)
		if duplicateErr == nil {
			_ = duplicate.Close()
			return errors.New("duplicate VRRP instance was accepted")
		}
		if !errors.Is(duplicateErr, unix.EADDRINUSE) {
			return fmt.Errorf("duplicate-instance check failed for another reason: %w", duplicateErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := &nativePeer{device: &failingTransport{nativeTransport: transport}, effects: effects, config: c, source: c.SourceAddress, done: make(chan error, 1), finished: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.finished:
		case <-time.After(3 * time.Second):
			t.Error("VRRP peer did not stop before descriptor cleanup")
			return
		}
		if err := p.device.Close(); err != nil {
			t.Errorf("close native transport: %v", err)
		}
	})
	go func() {
		defer close(p.finished)
		p.done <- run(ctx, c, transport.source, strings.Repeat(fmt.Sprint(number), 32), p.device, effects, nil)
	}()
	return p
}

func waitNativeState(t *testing.T, peer *nativePeer, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for peer.effects.state().State != state {
		select {
		case err := <-peer.done:
			t.Fatalf("peer stopped before %s: %v", state, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer did not enter %s; current=%+v", state, peer.effects.state())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nativeFixture(t *testing.T, family string) {
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "vrrp-lan"}}
	if err := netlink.LinkAdd(bridge); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(bridge) })
	if err := netlink.LinkSetUp(bridge); err != nil {
		t.Fatal(err)
	}
	high, low := makeNativePeer(t, bridge, family, 150, 1), makeNativePeer(t, bridge, family, 100, 2)
	waitNativeState(t, high, StateActive, 6*time.Second)
	waitNativeState(t, low, StateBackup, time.Second)
	// Observe at least two real advertisement intervals. A merely compiling
	// receiver with a broken multicast path would incorrectly take over here.
	time.Sleep(1200 * time.Millisecond)
	if low.effects.state().State != StateBackup {
		mdb, _ := exec.Command("bridge", "-s", "mdb", "show").CombinedOutput()
		t.Fatalf("backup did not receive active peer's native advertisements: low=%+v high=%+v MDB=%s", low.effects.state(), high.effects.state(), mdb)
	}
	// Fail all transmissions, including the graceful priority-zero packet.
	// The surviving peer must detect absence using its actual down timer.
	high.device.fail.Store(true)
	select {
	case err := <-high.done:
		if err == nil {
			t.Fatal("failed native sender unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed active sender did not stop")
	}
	waitNativeState(t, low, StateActive, 5*time.Second)
	if high.effects.state().Active || high.effects.state().State != StateInitialize {
		t.Fatal("failed peer did not withdraw its owned virtual link")
	}
	if err := low.device.VerifyActive(context.Background(), low.effects.state()); err != nil {
		t.Fatal("takeover did not install the virtual MAC and VIPs:", err)
	}
	low.cancel()
	select {
	case err := <-low.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("surviving peer did not shut down")
	}
	if low.effects.state().Active || low.effects.state().State != StateInitialize {
		t.Fatal("graceful shutdown retained ownership")
	}
	t.Logf("%s native raw-socket election, advertisement reception, VIP/VMAC verification and failover passed", family)
}

func (f *failingTransport) Advertise(ctx context.Context, p datagram) error {
	if f.fail.Load() {
		return unix.ENETDOWN
	}
	return f.nativeTransport.Advertise(ctx, p)
}
