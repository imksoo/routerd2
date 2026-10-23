//go:build linux && integration

package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Every mutating operation is in a re-executed test process with a freshly
// created user AND network namespace. The parent never invokes the engine.
// Set ROUTERD2_INTEGRATION_REQUIRED=1 in privileged CI to turn a namespace
// restriction into a failure instead of a skip.
func TestIsolatedKernelForwardingNATAndDSLite(t *testing.T) {
	if os.Getenv("ROUTERD2_NETNS_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		parent := os.Getenv("ROUTERD2_PARENT_NETNS")
		if parent == "" || current == parent {
			t.Fatal("refusing all mutations: test is not isolated from the parent network namespace")
		}
		runKernelFixture(t)
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
	cmd := exec.Command(exe, "-test.run=^TestIsolatedKernelForwardingNATAndDSLite$", "-test.v", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "ROUTERD2_NETNS_CHILD=1", "ROUTERD2_PARENT_NETNS="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	output, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		// Some restricted runtimes return ENOENT while Go is opening the
		// new child's uid_map/gid_map, rather than rejecting clone with EPERM.
		// Establish that the same binary can actually execute before treating
		// this as unavailable namespace infrastructure, never a passing test.
		if errors.Is(err, syscall.ENOENT) {
			probe := exec.Command(exe, "-test.run=^$")
			if probeErr := probe.Run(); probeErr == nil {
				unavailable = true
			}
		}
		if unavailable {
			if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
				t.Skipf("isolated user/network namespaces unavailable: %v", err)
			}
		}
		t.Fatalf("isolated kernel test: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func newPeerNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(original); err != nil {
		ns.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	return ns
}

func inNamespace(ns netns.NsHandle, fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return err
	}
	defer original.Close()
	if err = netns.Set(ns); err != nil {
		return err
	}
	defer func() {
		if restoreErr := netns.Set(original); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
	}()
	return fn()
}

func peerHandle(t *testing.T, ns netns.NsHandle) *netlink.Handle {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}
func peerAddress(t *testing.T, h *netlink.Handle, name, cidr string) {
	t.Helper()
	l, err := h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatal(err)
	}
	a.Flags = unix.IFA_F_NODAD
	if err := h.AddrAdd(l, a); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
}
func peerRoute(t *testing.T, h *netlink.Handle, name, destination, gateway string) {
	t.Helper()
	l, err := h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	_, prefix, err := net.ParseCIDR(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Dst: prefix, Gw: net.ParseIP(gateway)}); err != nil {
		t.Fatal(err)
	}
}

func runKernelFixture(t *testing.T) {
	clientNS, serverNS := newPeerNamespace(t), newPeerNamespace(t)
	for _, v := range []struct {
		name, peer string
		ns         netns.NsHandle
	}{{"lan0", "client0", clientNS}, {"wan0", "server0", serverNS}} {
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: v.name}, PeerName: v.peer, PeerNamespace: netlink.NsFd(v.ns)}); err != nil {
			t.Fatal(err)
		}
	}
	client, server := peerHandle(t, clientNS), peerHandle(t, serverNS)
	for _, a := range []string{"192.0.2.2/24", "2001:db8:1::2/64"} {
		peerAddress(t, client, "client0", a)
	}
	for _, a := range []string{"198.51.100.2/24", "2001:db8:2::2/64"} {
		peerAddress(t, server, "server0", a)
	}
	for _, a := range []string{"203.0.113.2/32", "2001:db8:3::2/128", "198.18.0.1/32"} {
		peerAddress(t, server, "lo", a)
	}
	peerRoute(t, client, "client0", "0.0.0.0/0", "192.0.2.1")
	peerRoute(t, client, "client0", "::/0", "2001:db8:1::1")
	foreign := &nftables.Conn{}
	foreign.AddTable(&nftables.Table{Name: "foreign_table", Family: nftables.TableFamilyINet})
	if err := foreign.Flush(); err != nil {
		t.Fatal(err)
	}
	lan, err := netlink.LinkByName("lan0")
	if err != nil {
		t.Fatal(err)
	}
	foreignAddr, _ := netlink.ParseAddr("203.0.113.250/32")
	if err := netlink.AddrAdd(lan, foreignAddr); err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{StatePath: filepath.Join(t.TempDir(), "network.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := Desired{
		Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24", "2001:db8:1::1/64"}}, {Name: "wan0", Addresses: []string{"198.51.100.1/24", "2001:db8:2::1/64"}}},
		Routes:     []Route{{Destination: "203.0.113.0/24", Gateway: "198.51.100.2", Interface: "wan0", Metric: 100}, {Destination: "2001:db8:3::/64", Gateway: "2001:db8:2::2", Interface: "wan0"}},
		Forwarding: Forwarding{IPv4: yes(), IPv6: yes()},
		Firewall:   &Firewall{Forward: []Forward{{From: "lan0", To: "wan0"}}, Masquerade: []Masquerade{{Source: "192.0.2.0/24", OutInterface: "wan0"}, {Source: "2001:db8:1::/64", OutInterface: "wan0"}}},
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	awaitIPv6(t, e)
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatalf("kernel reapply is not idempotent: %v", err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "203.0.113.2", "198.51.100.1")
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:2::1")
	peerRoute(t, server, "server0", "192.0.2.0/24", "198.51.100.1")
	assertNewReverseConnectionDropped(t, serverNS, clientNS)
	// A symmetric AFTR-side kernel tunnel makes this a real IPv4-in-IPv6
	// exchange, not just a tunnel-link existence assertion.
	underlay, err := server.LinkByName("server0")
	if err != nil {
		t.Fatal(err)
	}
	aftr := &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: "aftr0", MTU: 1460}, Link: uint32(underlay.Attrs().Index), Local: net.ParseIP("2001:db8:2::2"), Remote: net.ParseIP("2001:db8:2::1"), Proto: unix.IPPROTO_IPIP, Ttl: 64, Flags: 1}
	if err := server.LinkAdd(aftr); err != nil {
		t.Fatalf("kernel IPv6 tunnel support is required: %v", err)
	}
	peerAddress(t, server, "aftr0", "192.0.0.1/30")
	tunneled := cloneDesired(d)
	tunneled.DSLite = []DSLite{{Name: "dslite0", Underlay: "wan0", Local: "2001:db8:2::1", Remote: "2001:db8:2::2"}}
	tunneled.Interfaces = append(tunneled.Interfaces, Interface{Name: "dslite0", Addresses: []string{"192.0.0.2/30"}})
	tunneled.Routes = append(tunneled.Routes, Route{Destination: "198.18.0.0/24", Gateway: "192.0.0.1", Interface: "dslite0", Metric: 100})
	tunneled.Firewall.Forward = append(tunneled.Firewall.Forward, Forward{From: "lan0", To: "dslite0"})
	tunneled.Firewall.Masquerade = append(tunneled.Firewall.Masquerade, Masquerade{Source: "192.0.2.0/24", OutInterface: "dslite0"})
	if _, err := e.Apply(context.Background(), 2, tunneled); err != nil {
		t.Fatal(err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "198.18.0.1", "192.0.0.2")
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatalf("DS-Lite removal: %v", err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatalf("restore original network: %v", err)
	}
	addresses, err := netlink.AddrList(lan, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addresses {
		if a.IPNet.String() == foreignAddr.IPNet.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("foreign address was removed")
	}
	tables, err := (&nftables.Conn{}).ListTables()
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, table := range tables {
		if table.Name == "foreign_table" {
			found = true
		}
		if table.Name == FirewallTable {
			t.Fatal("owned table remains after rollback")
		}
	}
	if !found {
		t.Fatal("foreign nftables table was removed")
	}
}

func awaitIPv6(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := e.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		pending := false
		for _, l := range snapshot.Interfaces {
			for _, a := range l.Addresses {
				if !a.Owned {
					continue
				}
				if a.Flags&unix.IFA_F_DADFAILED != 0 {
					t.Fatalf("DAD failed: %s", a.Address)
				}
				pending = pending || a.Flags&unix.IFA_F_TENTATIVE != 0
			}
		}
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("IPv6 DAD did not complete")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func tcpExchange(t *testing.T, client, server netns.NsHandle, network, address, wantSource string) {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(server, func() error {
		var err error
		listener, err = net.Listen(network, net.JoinHostPort(address, "0"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		source, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err != nil || source != wantSource {
			result <- fmt.Errorf("source=%s want=%s err=%v", source, wantSource, err)
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			result <- err
			return
		}
		_, err = conn.Write(payload)
		result <- err
	}()
	if err := inNamespace(client, func() error {
		dialer := net.Dialer{Timeout: 3 * time.Second}
		conn, err := dialer.Dial(network, listener.Addr().String())
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("test")); err != nil {
			return err
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return err
		}
		if string(payload) != "test" {
			return fmt.Errorf("unexpected echo %q", payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func assertNewReverseConnectionDropped(t *testing.T, server, client netns.NsHandle) {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(client, func() error { var err error; listener, err = net.Listen("tcp4", "192.0.2.2:0"); return err }); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	err := inNamespace(server, func() error {
		d := net.Dialer{Timeout: 400 * time.Millisecond}
		c, err := d.Dial("tcp4", listener.Addr().String())
		if c != nil {
			c.Close()
		}
		return err
	})
	if err == nil {
		t.Fatal("unsolicited WAN-to-LAN connection passed default-drop forward chain")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("reverse test failed for a reason other than firewall drop: %v", err)
	}
	if strings.Contains(err.Error(), "network is unreachable") {
		t.Fatal("invalid reverse-route fixture")
	}
}
