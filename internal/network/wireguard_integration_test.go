//go:build linux && integration

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// A fault-injection adapter still writes the REAL kernel. It emulates loss of
// the worker between bounded SET batches after a known peer prefix subset was
// acknowledged, then lets a fresh engine recover from the on-disk intent.
type interruptedNativeWireGuard struct{ *linuxKernel }

func (k *interruptedNativeWireGuard) ReplaceWireGuard(index int, wanted wireGuardDevice) error {
	partial := cloneWireGuardDevice(wanted)
	defer clearWireGuardDevice(&partial)
	if len(partial.Peers) > 0 && len(partial.Peers[0].AllowedIPs) > 128 {
		partial.Peers[0].AllowedIPs = partial.Peers[0].AllowedIPs[:128]
	}
	if err := k.linuxKernel.ReplaceWireGuard(index, partial); err != nil {
		return err
	}
	return &partialMutationError{errors.New("injected interruption after acknowledged real-kernel peer subset")}
}

type failedNativeWireGuardUp struct {
	*linuxKernel
	afterUp func() error
}

func (k *failedNativeWireGuardUp) SetWireGuardUp(index int, up bool) error {
	if err := k.linuxKernel.SetWireGuardUp(index, up); err != nil {
		return err
	}
	if up && k.afterUp != nil {
		hook := k.afterUp
		k.afterUp = nil
		return hook()
	}
	return nil
}

func TestIsolatedKernelWireGuardOwnershipAndTraffic(t *testing.T) {
	const name = "TestIsolatedKernelWireGuardOwnershipAndTraffic"
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ROUTERD2_WIREGUARD_CHILD") == "1" {
		if parent := os.Getenv("ROUTERD2_PARENT_NETNS"); parent == "" || current == parent {
			t.Fatal("refusing WireGuard mutations outside the isolated fixture namespace")
		}
		runWireGuardKernelFixture(t)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^"+name+"$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "ROUTERD2_WIREGUARD_CHILD=1", "ROUTERD2_PARENT_NETNS="+current)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	output, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		if errors.Is(err, syscall.ENOENT) && exec.Command(exe, "-test.run=^$").Run() == nil {
			unavailable = true
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated namespaces unavailable: %v", err)
		}
		t.Fatalf("isolated WireGuard test: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func runWireGuardKernelFixture(t *testing.T) {
	ctx := context.Background()
	peerNS := newPeerNamespace(t)
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wgwan"}, PeerName: "wgpeerwan", PeerNamespace: netlink.NsFd(peerNS)}); err != nil {
		t.Fatal(err)
	}
	peer := peerHandle(t, peerNS)
	peerAddress(t, peer, "wgpeerwan", "192.0.2.2/24")
	peerAddress(t, peer, "wgpeerwan", "2001:db8:99::2/64")
	device := fixtureWireGuardDesired(t)
	device.WireGuard[0].ListenPort = 51820
	device.WireGuard[0].Peers[0].Endpoint = "192.0.2.2:51821"
	device.WireGuard[0].Peers[0].PersistentKeepalive = 0
	device.Links[0].IPv6AddrGenMode = "none"
	device.Interfaces = append([]Interface{{Name: "wgwan", Addresses: []string{"192.0.2.1/24", "2001:db8:99::1/64"}, AddressOptions: map[string]AddressOptions{"2001:db8:99::1/64": {NoDAD: true}}}}, device.Interfaces...)
	device.Interfaces[1].Addresses = append(device.Interfaces[1].Addresses, "fd00:99::1/64")
	device.Interfaces[1].AddressOptions = map[string]AddressOptions{"fd00:99::1/64": {NoDAD: true}}
	private, psk := fixtureWireGuardKey(81), fixtureWireGuardKey(73)
	public, _ := private.public()
	routerPublic, _ := fixtureWireGuardKey(31).public()
	peerDevice := wireGuardDevice{PrivateKey: private, PublicKey: public, ListenPort: 51821, Peers: []wireGuardNativePeer{{PublicKey: routerPublic, PresharedKey: psk, AllowedIPs: []string{"10.99.0.1/32", "fd00:99::1/128"}, Endpoint: "192.0.2.1:51820"}}}
	k := &linuxKernel{}
	if err := inNamespace(peerNS, func() error {
		link := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "peerwg"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		peerDevice.Index = link.Attrs().Index
		return k.ReplaceWireGuard(peerDevice.Index, peerDevice)
	}); err != nil {
		t.Fatal("independent peer setup:", err)
	}
	peerAddress(t, peer, "peerwg", "10.99.0.2/24")
	peerAddress(t, peer, "peerwg", "fd00:99::2/64")
	journalPath := filepath.Join(t.TempDir(), "network.json")
	e, err := New(Options{StatePath: journalPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	snapshot, err := e.Apply(ctx, 1, device)
	if err != nil {
		t.Fatal("initial native WireGuard:", err)
	}
	if len(snapshot.WireGuard) != 1 || !snapshot.WireGuard[0].Owned || snapshot.WireGuard[0].Quarantined {
		t.Fatal("missing native WireGuard ownership state")
	}
	routerNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer routerNS.Close()
	traffic := func() {
		t.Helper()
		tcpExchange(t, routerNS, peerNS, "tcp4", "10.99.0.2", "10.99.0.1")
		tcpExchange(t, routerNS, peerNS, "tcp6", "fd00:99::2", "fd00:99::1")
	}
	traffic()
	snapshot, err = e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stats := snapshot.WireGuard[0].Peers[0]
	if stats.LastHandshake.IsZero() || stats.ReceivedBytes == 0 || stats.TransmittedBytes == 0 {
		t.Fatal("encrypted exchanges did not produce handshake/counters")
	}
	index := snapshot.WireGuard[0].Index
	t.Log("Linux WireGuard IPv4/IPv6 encrypted TCP, public status, handshake and counters passed")

	// Exercise real roaming: change the peer's UDP source port and initiate
	// authenticated traffic from that peer before updating the router config.
	peerDevice.ListenPort = 51822
	if err := inNamespace(peerNS, func() error {
		if err := k.SetWireGuardUp(peerDevice.Index, false); err != nil {
			return err
		}
		if err := k.ReplaceWireGuard(peerDevice.Index, peerDevice); err != nil {
			return err
		}
		return k.SetWireGuardUp(peerDevice.Index, true)
	}); err != nil {
		t.Fatal(err)
	}
	// Linux removes IPv6 addresses on a DOWN transition with the default
	// keep_addr_on_down policy. The independent fixture reapplies its own
	// address, just as the router engine reconciles its owned addresses.
	peerLink, err := peer.LinkByName("peerwg")
	if err != nil {
		t.Fatal(err)
	}
	peerV6, _ := netlink.ParseAddr("fd00:99::2/64")
	peerV6.Flags = unix.IFA_F_NODAD
	if err := peer.AddrReplace(peerLink, peerV6); err != nil {
		t.Fatal(err)
	}
	tcpExchange(t, peerNS, routerNS, "tcp4", "10.99.0.1", "10.99.0.2")
	live, err := k.ReadWireGuard(index)
	if err != nil {
		t.Fatal(err)
	}
	if live.Peers[0].Endpoint != "192.0.2.2:51822" {
		t.Fatal("kernel did not learn the new authenticated roaming endpoint")
	}
	clearWireGuardDevice(&live)
	updated := cloneDesired(device)
	updated.WireGuard[0].Peers[0].PersistentKeepalive = 20
	// A large single peer spans both SET batches and multipart GET fragments.
	for i := 0; i < 2048; i++ {
		updated.WireGuard[0].Peers[0].AllowedIPs = append(updated.WireGuard[0].Peers[0].AllowedIPs, fmt.Sprintf("198.18.%d.%d/32", i/256, i%256))
	}
	e.kernel = &interruptedNativeWireGuard{k}
	if _, err := e.Apply(ctx, 2, updated); err == nil {
		t.Fatal("interrupted real-kernel replacement succeeded")
	}
	interrupted, err := netlink.LinkByIndex(index)
	if err != nil || interrupted.Attrs().Flags&net.FlagUp != 0 {
		t.Fatal("interrupted real-kernel replacement was not kept DOWN", err)
	}
	live, err = k.ReadWireGuard(index)
	if err != nil || len(live.Peers) != 1 || len(live.Peers[0].AllowedIPs) != 128 {
		t.Fatal("fault injection did not leave a real partial peer set", err)
	}
	clearWireGuardDevice(&live)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = New(Options{StatePath: journalPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, 1, device); err != nil {
		t.Fatal("real partial-set restart rollback:", err)
	}
	traffic()
	t.Log("interrupted actual kernel peer subset stayed DOWN; a fresh engine rolled back and restored IPv4/IPv6 traffic")
	for _, stage := range []string{"lost UP acknowledgment", "canceled after UP"} {
		applyCtx, cancel := context.WithCancel(ctx)
		e.kernel = &failedNativeWireGuardUp{linuxKernel: k, afterUp: func() error {
			if stage == "canceled after UP" {
				cancel()
				return nil
			}
			return errors.New("injected lost real-kernel UP acknowledgment")
		}}
		_, applyErr := e.Apply(applyCtx, 2, updated)
		cancel()
		if applyErr == nil {
			t.Fatal("real-kernel post-UP failure was acknowledged", stage)
		}
		closed, err := netlink.LinkByIndex(index)
		if err != nil || closed.Attrs().Flags&net.FlagUp != 0 {
			t.Fatal("native quarantine left the owned device UP", stage, err)
		}
		e.kernel = k
		if _, err := e.Apply(ctx, 1, device); err != nil {
			t.Fatal("post-UP failure rollback:", stage, err)
		}
		traffic()
	}
	t.Log("lost UP acknowledgment and canceled apply both triggered independent native quarantine; rollback restored both families")
	if _, err := e.Apply(ctx, 2, updated); err != nil {
		t.Fatal("multipart WireGuard replacement:", err)
	}
	live, err = k.ReadWireGuard(index)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Peers) != 1 || len(live.Peers[0].AllowedIPs) != 2050 || live.Peers[0].Endpoint != "192.0.2.2:51822" {
		t.Fatal("multipart replacement lost prefixes or roaming endpoint")
	}
	clearWireGuardDevice(&live)
	if _, err := e.Apply(ctx, 2, updated); err != nil {
		t.Fatal("multipart readback reapply:", err)
	}
	traffic()
	routes, err := k.Routes(254)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if strings.HasPrefix(route.Destination, "198.18.") {
			t.Fatal("AllowedIPs implicitly installed a FIB route")
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = New(Options{StatePath: journalPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, 2, updated); err != nil {
		t.Fatal("WireGuard journal restart:", err)
	}
	traffic()
	t.Log("authenticated roaming survived 2050-prefix multipart replacement, idempotent apply and engine restart; no implicit FIB routes")

	// Switch the outer transport to IPv6 as an explicit endpoint update. Both
	// inner IP families must still work, and the peer owns/reapplies its IPv6
	// address after taking its device down.
	peerDevice.Peers[0].Endpoint = "[2001:db8:99::1]:51820"
	if err := inNamespace(peerNS, func() error {
		if err := k.SetWireGuardUp(peerDevice.Index, false); err != nil {
			return err
		}
		if err := k.ReplaceWireGuard(peerDevice.Index, peerDevice); err != nil {
			return err
		}
		return k.SetWireGuardUp(peerDevice.Index, true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := peer.AddrReplace(peerLink, peerV6); err != nil {
		t.Fatal(err)
	}
	updated.WireGuard[0].Peers[0].Endpoint = "[2001:db8:99::2]:51822"
	if _, err := e.Apply(ctx, 3, updated); err != nil {
		t.Fatal("IPv6 outer transport update:", err)
	}
	traffic()
	t.Log("explicit IPv6 UDP endpoints carried both IPv4 and IPv6 encrypted TCP")

	// A mismatched PSK must prevent new encrypted data, then the previous
	// immutable key version must restore communication without wg/ip helpers.
	wrong := cloneDesired(updated)
	wrongPath := filepath.Join(t.TempDir(), "wrong-psk")
	wrongKey := fixtureWireGuardKey(93)
	if err := os.WriteFile(wrongPath, []byte(wrongKey.encoded()), 0600); err != nil {
		t.Fatal(err)
	}
	wrong.WireGuard[0].Peers[0].PresharedKeyFile = wrongPath
	if _, err := e.Apply(ctx, 3, wrong); err != nil {
		t.Fatal(err)
	}
	assertWireGuardTCPUnavailable(t, peerNS, "10.99.0.2")
	if _, err := e.Apply(ctx, 2, updated); err != nil {
		t.Fatal("PSK rollback:", err)
	}
	traffic()
	removed := cloneDesired(updated)
	removed.WireGuard[0].Peers = nil
	if _, err := e.Apply(ctx, 4, removed); err != nil {
		t.Fatal("peer removal:", err)
	}
	live, err = k.ReadWireGuard(index)
	if err != nil || len(live.Peers) != 0 {
		t.Fatal("peer removal was not acknowledged", err)
	}
	clearWireGuardDevice(&live)
	assertWireGuardTCPUnavailable(t, peerNS, "10.99.0.2")
	if _, err := e.Apply(ctx, 5, updated); err != nil {
		t.Fatal("peer recreation:", err)
	}
	traffic()
	t.Log("PSK mismatch and peer removal prevented traffic; immutable-secret rollback and peer recreation restored both IP families")

	// A separate actor adding a peer must cause a refusal without taking the
	// interface down or deleting that peer. Clean up only after restoring the
	// test's previous exact native configuration itself.
	ours, err := k.ReadWireGuard(index)
	if err != nil {
		t.Fatal(err)
	}
	foreign := cloneWireGuardDevice(ours)
	foreignKey, _ := fixtureWireGuardKey(98).public()
	foreign.Peers = append(foreign.Peers, wireGuardNativePeer{PublicKey: foreignKey, AllowedIPs: []string{"203.0.113.0/24"}})
	if err := k.SetWireGuardUp(index, false); err != nil {
		t.Fatal(err)
	}
	if err := k.ReplaceWireGuard(index, foreign); err != nil {
		t.Fatal(err)
	}
	if err := k.SetWireGuardUp(index, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, 5, updated); err == nil {
		t.Fatal("foreign peer was silently adopted")
	}
	link, err := netlink.LinkByIndex(index)
	if err != nil || link.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("foreign peer caused an unauthorized DOWN transition", err)
	}
	live, err = k.ReadWireGuard(index)
	if err != nil || len(live.Peers) != 2 {
		t.Fatal("foreign peer disappeared", err)
	}
	clearWireGuardDevice(&live)
	if err := k.SetWireGuardUp(index, false); err != nil {
		t.Fatal(err)
	}
	if err := k.ReplaceWireGuard(index, ours); err != nil {
		t.Fatal(err)
	}
	if err := k.SetWireGuardUp(index, true); err != nil {
		t.Fatal(err)
	}
	clearWireGuardDevice(&ours)
	clearWireGuardDevice(&foreign)
	if _, err := e.Apply(ctx, 6, Desired{}); err != nil {
		t.Fatal("WireGuard native cleanup:", err)
	}
	if _, err := netlink.LinkByName("wg0"); err == nil {
		t.Fatal("owned WireGuard device survived removal")
	}
	t.Log("foreign peer remained untouched and UP; owned device, addresses and journal entries removed after explicit cleanup")
}

func assertWireGuardTCPUnavailable(t *testing.T, peerNS netns.NsHandle, address string) {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(peerNS, func() error {
		var err error
		listener, err = net.Listen("tcp4", net.JoinHostPort(address, "0"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialer := net.Dialer{Timeout: 600 * time.Millisecond}
	conn, err := dialer.Dial("tcp4", listener.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("WireGuard traffic passed with an absent peer or wrong PSK")
	}
}
