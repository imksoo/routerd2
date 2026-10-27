//go:build linux && integration

package network

import (
	"bytes"
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
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/ipv6"
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
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
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
	runNativePolicyFixture(t, e, d, clientNS, serverNS)
	runNativeECMPFixture(t, e, d, clientNS, serverNS)
	runNativeACLNATFixture(t, e, d, clientNS, serverNS)
	runNativeIPv4FragmentFixture(t, e, d, clientNS, serverNS)
	runNativeRAStateFixture(t, e, d, clientNS, serverNS)
	runNativeAddressLifetimeFixture(t, e, d)
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
	tunneled.Links = []Link{{Name: "dslite0", Kind: "ip6tnl", Parent: "wan0", Local: "2001:db8:2::1", Remote: "2001:db8:2::2"}}
	tunneled.Interfaces = append(tunneled.Interfaces, Interface{Name: "dslite0", Addresses: []string{"192.0.0.2/32"}, AddressOptions: map[string]AddressOptions{"192.0.0.2/32": {Peer: "192.0.0.1/32"}}})
	tunneled.Routes = append(tunneled.Routes, Route{Destination: "198.18.0.0/24", Gateway: "192.0.0.1", Interface: "dslite0", Metric: 100})
	tunneled.Firewall.Forward = append(tunneled.Firewall.Forward, Forward{From: "lan0", To: "dslite0"})
	tunneled.Firewall.Masquerade = append(tunneled.Firewall.Masquerade, Masquerade{Source: "192.0.2.0/24", OutInterface: "dslite0"})
	if _, err := e.Apply(context.Background(), 2, tunneled); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 2, tunneled); err != nil {
		t.Fatalf("DS-Lite reapply: %v", err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "198.18.0.1", "192.0.0.2")
	if err := server.LinkSetMTU(underlay, 1280); err != nil {
		t.Fatal(err)
	}
	if err := server.LinkSetMTU(aftr, 1240); err != nil {
		t.Fatal(err)
	}
	tunneled.Interfaces[1].MTU = 1280
	tunneled.Interfaces[len(tunneled.Interfaces)-1].MTU = 1460
	tunneled.Firewall.IPv4ClearDF = []IPv4ClearDF{{From: "lan0", To: "dslite0"}}
	for range 2 {
		state, err := e.Apply(context.Background(), 3, tunneled)
		if err != nil {
			t.Fatal(err)
		}
		for _, iface := range state.Interfaces {
			if iface.Name == "dslite0" && iface.MTU != 1240 {
				t.Fatalf("DS-Lite did not follow its live underlay: %d", iface.MTU)
			}
		}
		nativeUDPExchange(t, clientNS, serverNS, "udp4", "192.0.2.2:0", "198.18.0.1:47004", "192.0.0.2", PortRange{Start: 1, End: 65535})
		tcpExchange(t, clientNS, serverNS, "tcp4", "198.18.0.1", "192.0.0.2")
	}
	if err := server.LinkSetMTU(underlay, 1500); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatalf("DS-Lite removal: %v", err)
	}
	t.Log("native DS-Lite TCP/UDP, live underlay1280/tunnel1240, reapply and removal passed")
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
	runNativeLinkFixture(t)
}

func runNativeECMPFixture(t *testing.T, e *Engine, base Desired, clientNS, serverNS netns.NsHandle) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wan1"}, PeerName: "ecmp1", PeerNamespace: netlink.NsFd(serverNS)}); err != nil {
		t.Fatal(err)
	}
	wan1, err := netlink.LinkByName("wan1")
	if err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(wan1)
	server := peerHandle(t, serverNS)
	peerAddress(t, server, "ecmp1", "198.51.101.2/24")
	peerAddress(t, server, "ecmp1", "2001:db8:22::2/64")
	d := cloneDesired(base)
	d.Interfaces = append(d.Interfaces, Interface{Name: "wan1", Addresses: []string{"198.51.101.1/24", "2001:db8:22::1/64"}})
	d.Firewall.Forward = append(d.Firewall.Forward, Forward{From: "lan0", To: "wan1"})
	d.Firewall.Masquerade = append(d.Firewall.Masquerade, Masquerade{Source: "192.0.2.0/24", OutInterface: "wan1"}, Masquerade{Source: "2001:db8:1::/64", OutInterface: "wan1"})
	d.Routes[0].Gateway, d.Routes[0].Interface = "", ""
	d.Routes[0].NextHops = []NextHop{{Gateway: "198.51.100.2", Interface: "wan0", Weight: 1}, {Gateway: "198.51.101.2", Interface: "wan1", Weight: 1}}
	d.Routes[1].Gateway, d.Routes[1].Interface = "", ""
	d.Routes[1].NextHops = []NextHop{{Gateway: "2001:db8:2::2", Interface: "wan0", Weight: 1}, {Gateway: "2001:db8:22::2", Interface: "wan1", Weight: 1}}
	d.Routes[1].ValidUntil = lifetime.Deadline{UTC: time.Now().Add(time.Minute)}
	if _, err := e.Apply(context.Background(), 20, d); err != nil {
		t.Fatalf("ECMP install: %v", err)
	}
	awaitIPv6(t, e)
	check := func(want Desired) {
		t.Helper()
		s, err := e.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range want.Routes {
			found := false
			for _, live := range s.Routes {
				if routeMatches(live, testOwnedRoute(t, route)) && live.Owned {
					found = true
				}
			}
			if !found {
				t.Fatalf("native ECMP weights/ownership differ: want=%+v live=%+v", route, s.Routes)
			}
		}
	}
	check(d)
	if _, err := e.Apply(context.Background(), 20, d); err != nil {
		t.Fatalf("ECMP reapply: %v", err)
	}
	// Default Linux ECMP hashing includes IP addresses. Vary destination hosts
	// and prove that both egress NICs carry complete TCP exchanges in each
	// family. Source NAT makes the selected egress observable at the receiver.
	for _, family := range []int{4, 6} {
		sources := map[string]bool{}
		for i := 2; i < 66 && len(sources) < 2; i++ {
			address, prefix, protocol := fmt.Sprintf("203.0.113.%d", i), "/32", "tcp4"
			allowed := []string{"198.51.100.1", "198.51.101.1"}
			if family == 6 {
				address, prefix, protocol = fmt.Sprintf("2001:db8:3::%x", i), "/128", "tcp6"
				allowed = []string{"2001:db8:2::1", "2001:db8:22::1"}
			}
			if i > 2 {
				peerAddress(t, server, "lo", address+prefix)
			}
			sources[tcpExchange(t, clientNS, serverNS, protocol, address, allowed...)] = true
		}
		if len(sources) != 2 {
			t.Fatalf("IPv%d ECMP did not carry packets on both NICs: %v", family, sources)
		}
	}
	for i := range d.Routes {
		d.Routes[i].NextHops[0].Weight, d.Routes[i].NextHops[1].Weight = 256, 3
	}
	if _, err := e.Apply(context.Background(), 21, d); err != nil {
		t.Fatalf("weighted ECMP update: %v", err)
	}
	check(d)
	if _, err := e.Apply(context.Background(), 21, d); err != nil {
		t.Fatalf("weighted ECMP reapply: %v", err)
	}
	foreign := Route{Destination: "198.19.7.0/24", Metric: 177, NextHops: []NextHop{{Gateway: "198.51.100.2", Interface: "wan0", Weight: 2}, {Gateway: "198.51.101.2", Interface: "wan1", Weight: 5}}}
	native, err := linuxRoute(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteAdd(native); err != nil {
		t.Fatal(err)
	}
	defer netlink.RouteDel(native)
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatalf("ECMP rollback to scalar: %v", err)
	}
	live, err := e.kernel.Routes(254)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, route := range live {
		if sameKernelRoute(route.Route, foreign) {
			found = true
			if e.ownsRoute(route) {
				t.Fatal("foreign ECMP route with matching protocol was adopted")
			}
		}
	}
	if !found {
		t.Fatal("foreign ECMP route was removed")
	}
	t.Log("IPv4/IPv6 ECMP TCP over both NICs, IPv6 kernel expiry, weights 256/3, reapply, rollback and foreign route preservation passed")
}

func runNativeIPv4FragmentFixture(t *testing.T, e *Engine, base Desired, client, server netns.NsHandle) {
	t.Helper()
	peer, err := netlink.NewHandleAt(server, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	link, err := peer.LinkByName("server0")
	if err != nil {
		t.Fatal(err)
	}
	before := link.Attrs().MTU
	defer func() {
		if err := peer.LinkSetMTU(link, before); err != nil {
			t.Error(err)
		}
	}()
	d := cloneDesired(base)
	d.Firewall.IPv4ClearDF = []IPv4ClearDF{{From: "lan0", To: "wan0"}}
	for _, mtu := range []int{1454, 1400} {
		if err := peer.LinkSetMTU(link, mtu); err != nil {
			t.Fatal(err)
		}
		d.Interfaces[1].MTU = mtu
		if _, err := e.Apply(context.Background(), 30, d); err != nil {
			t.Fatal(err)
		}
		nativeUDPExchange(t, client, server, "udp4", "192.0.2.2:0", "203.0.113.2:47002", "198.51.100.1", PortRange{Start: 1, End: 65535})
		flow := openNativeTCP(t, client, server, "tcp4", "203.0.113.2:47003", "", "198.51.100.1", nil)
		defer flow.close()
		flow.exchange(t)
		for _, conn := range []net.Conn{flow.client, flow.server} {
			raw, err := conn.(*net.TCPConn).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var mss int
			var optionErr error
			if err := raw.Control(func(fd uintptr) {
				mss, optionErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
			}); err != nil || optionErr != nil || mss > mtu-40 || mss < 536 {
				t.Fatalf("MTU %d TCP MSS %d: %v %v", mtu, mss, err, optionErr)
			}
		}
		flow.close()
		t.Logf("native IPv4 MTU %d derived TCP MSS and DF clearing; 1450-byte UDP reassembly passed", mtu)
	}
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatal(err)
	}
}

func runNativeRAStateFixture(t *testing.T, e *Engine, base Desired, clientNS, serverNS netns.NsHandle) {
	t.Helper()
	acceptKey := settingKey{Kind: "accept_ra", Interface: "wan0"}
	autoconfKey := settingKey{Kind: "autoconf", Interface: "wan0"}
	beforeAccept, err := e.kernel.ReadSetting(acceptKey)
	if err != nil {
		t.Fatal(err)
	}
	beforeAutoconf, err := e.kernel.ReadSetting(autoconfKey)
	if err != nil {
		t.Fatal(err)
	}
	d := cloneDesired(base)
	zero, disabled := 0, false
	d.Interfaces[1].IPv6AcceptRA, d.Interfaces[1].IPv6Autoconf = &zero, &disabled
	d.Links = append(d.Links, Link{Name: "rabr0", Kind: "bridge", IPv6AddrGenMode: "none"})
	d.Interfaces = append(d.Interfaces, Interface{Name: "rabr0", IPv6AcceptRA: &zero, IPv6Autoconf: &disabled})
	d.Routes[1].ValidUntil = lifetime.Deadline{UTC: time.Now().Add(4 * time.Second)}
	d.Routes = append(d.Routes, Route{Destination: "2001:db8:77::/64", Interface: "wan0", ValidUntil: lifetime.Deadline{UTC: time.Now().Add(time.Minute)}})
	if _, err := e.Apply(context.Background(), 40, d); err != nil {
		t.Fatalf("RA writer settings and finite routes: %v", err)
	}
	for _, name := range []string{"wan0", "rabr0"} {
		l, err := e.kernel.Interface(name)
		if err != nil || l.IPv6AcceptRA == nil || *l.IPv6AcceptRA != 0 || l.IPv6Autoconf == nil || *l.IPv6Autoconf || !l.Up {
			t.Fatalf("kernel RA/SLAAC writers were not disabled on %s: %+v %v", name, l, err)
		}
	}
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:2::1")
	d.Routes[1].ValidUntil = lifetime.Deadline{UTC: time.Now().Add(9 * time.Second)}
	if _, err := e.Apply(context.Background(), 41, d); err != nil {
		t.Fatalf("finite route renewal: %v", err)
	}
	if _, err := e.Apply(context.Background(), 41, d); err != nil {
		t.Fatalf("finite route reapply: %v", err)
	}
	observed, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range d.Routes {
		found := false
		for _, live := range observed.Routes {
			found = found || routeMatches(live, testOwnedRoute(t, want)) && live.Owned
		}
		if !found {
			t.Fatalf("finite route deadline or ownership not read back: want %+v; live %+v", want, observed.Routes)
		}
	}
	foreign := Route{Destination: "2001:db8:99::/64", Interface: "wan0", Metric: 177, ValidUntil: lifetime.Deadline{UTC: time.Now().Add(time.Hour)}}
	if err := e.kernel.AddRoute(testOwnedRoute(t, foreign)); err != nil {
		t.Fatal(err)
	}
	defer e.kernel.DeleteRoute(foreign)
	// No Apply call runs while this deadline passes. FIB lookup must stop
	// selecting the expired route without depending on the control process.
	target := net.ParseIP("2001:db8:3::2")
	if _, err := netlink.RouteGet(target); err != nil {
		t.Fatalf("finite route expired before its deadline: %v", err)
	}
	limit := d.Routes[1].ValidUntil.UTC.Add(2 * time.Second)
	for {
		_, err := netlink.RouteGet(target)
		if errors.Is(err, unix.ENETUNREACH) {
			break
		}
		if err != nil || time.Now().After(limit) {
			t.Fatalf("kernel FIB retained expired IPv6 route without controller: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := e.Apply(context.Background(), 42, d); err != nil {
		t.Fatalf("expired route projection: %v", err)
	}
	for _, r := range e.state.Desired.Routes {
		if r.Destination == "2001:db8:3::/64" {
			t.Fatal("expired route retained in desired projection")
		}
	}
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatalf("restore RA writer settings: %v", err)
	}
	if value, err := e.kernel.ReadSetting(acceptKey); err != nil || value != beforeAccept {
		t.Fatalf("original accept_ra not restored: %d %v", value, err)
	}
	if value, err := e.kernel.ReadSetting(autoconfKey); err != nil || value != beforeAutoconf {
		t.Fatalf("original autoconf not restored: %d %v", value, err)
	}
	routes, err := e.kernel.Routes(254)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range routes {
		if routeMatches(r, testOwnedRoute(t, foreign)) {
			found = true
			if e.ownsRoute(r) {
				t.Fatal("foreign finite route with matching protocol adopted")
			}
		}
	}
	if !found {
		t.Fatal("foreign finite route was removed")
	}
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:2::1")
	t.Log("RA/SLAAC writer ownership/restoration, finite IPv6 on-link and gateway routes, renewal, autonomous FIB expiry and foreign finite route preservation passed")
}

func runNativeAddressLifetimeFixture(t *testing.T, e *Engine, base Desired) {
	t.Helper()
	d := cloneDesired(base)
	address := "2001:db8:2::1/64"
	now := time.Now()
	o := AddressOptions{PreferredUntil: now.Add(time.Hour), ValidUntil: now.Add(2 * time.Hour)}
	d.Interfaces[1].AddressOptions = map[string]AddressOptions{address: o}
	if _, err := e.Apply(context.Background(), 4, d); err != nil {
		t.Fatalf("finite address lifetime: %v", err)
	}
	o.PreferredUntil = now.Add(-time.Second)
	d.Interfaces[1].AddressOptions[address] = o
	if _, err := e.Apply(context.Background(), 5, d); err != nil {
		t.Fatalf("deprecate owned prefix address: %v", err)
	}
	link, err := e.kernel.Interface("wan0")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range link.Addresses {
		if a.Address == address {
			found = true
			if a.PreferredLifetime != 0 || a.ValidLifetime < 7100 || a.Flags&unix.IFA_F_DEPRECATED == 0 {
				t.Fatalf("kernel did not deprecate retained address: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("deprecated address was removed early")
	}
	o.ValidUntil = now.Add(-time.Second)
	o.PreferredUntil = now.Add(-2 * time.Second)
	d.Interfaces[1].AddressOptions[address] = o
	if _, err := e.Apply(context.Background(), 6, d); err != nil {
		t.Fatalf("expire owned prefix address: %v", err)
	}
	link, err = e.kernel.Interface("wan0")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range link.Addresses {
		if a.Address == address {
			t.Fatal("expired address remains")
		}
	}
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatalf("restore static address lifetime: %v", err)
	}
	awaitIPv6(t, e)
	t.Log("native preferred/valid lifetime update, deprecation, expiry and static restoration passed")
}

func runNativePolicyFixture(t *testing.T, e *Engine, base Desired, clientNS, serverNS netns.NsHandle) {
	t.Helper()
	foreign := netlink.NewRule()
	foreign.Family = unix.AF_INET
	foreign.Priority = 22000
	foreign.Table = 200
	foreign.Protocol = 4
	_, foreign.Src, _ = net.ParseCIDR("192.0.2.33/32")
	if err := netlink.RuleAdd(foreign); err != nil {
		t.Fatal(err)
	}
	d := cloneDesired(base)
	for i := range d.Routes {
		d.Routes[i].Table = 100
	}
	d.Routes = append(d.Routes, Route{Destination: "198.19.1.0/24", Interface: "wan0", Table: 101, Metric: 10})
	d.PolicyRules = []PolicyRule{
		{Family: 4, Priority: 11000, Table: 101, Mark: uint32Pointer(0x55), Mask: uint32Pointer(0xff), InInterface: "lan0"},
		{Family: 4, Priority: 12000, Table: 100, Source: "192.0.2.0/24", InInterface: "lan0"},
		{Family: 6, Priority: 12000, Table: 100, Source: "2001:db8:1::/64", InInterface: "lan0"},
	}
	if _, err := e.Apply(context.Background(), 3, d); err != nil {
		t.Fatalf("native policy routing: %v", err)
	}
	if _, err := e.Apply(context.Background(), 3, d); err != nil {
		t.Fatalf("native policy reapply: %v", err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "203.0.113.2", "198.51.100.1")
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:2::1")
	routes, err := netlink.RouteGetWithOptions(net.ParseIP("198.19.1.4"), &netlink.RouteGetOptions{Iif: "lan0", SrcAddr: net.ParseIP("192.0.2.2"), Mark: 0x55, FIBMatch: true})
	if err != nil || len(routes) != 1 || routes[0].Table != 101 {
		t.Fatalf("marked route lookup did not select table 101: %+v %v", routes, err)
	}
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatalf("native policy cleanup: %v", err)
	}
	rules, err := e.kernel.PolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rules {
		if r.Priority == 22000 && r.Family == 4 && r.Protocol == 4 {
			found = true
		}
		if r.Protocol == RouteProtocol {
			t.Fatalf("owned policy rule remains after removal: %+v", r)
		}
	}
	if !found {
		t.Fatal("foreign policy rule removed")
	}
	t.Log("IPv4/IPv6 source-policy TCP and marked table lookup passed; foreign policy rule preserved")
}

func runNativeLinkFixture(t *testing.T) {
	t.Helper()
	clientNS, serverNS := newPeerNamespace(t), newPeerNamespace(t)
	for _, v := range []struct {
		name, peer string
		ns         netns.NsHandle
	}{{"member0", "peerm0", clientNS}, {"uplink0", "peerup0", serverNS}} {
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: v.name}, PeerName: v.peer, PeerNamespace: netlink.NsFd(v.ns)}); err != nil {
			t.Fatal(err)
		}
	}
	client, server := peerHandle(t, clientNS), peerHandle(t, serverNS)
	peerAddress(t, client, "peerm0", "10.77.0.2/24")
	peerAddress(t, server, "peerup0", "10.77.0.3/24")
	member, err := netlink.LinkByName("member0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetAlias(member, "preexisting member"); err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{StatePath: filepath.Join(t.TempDir(), "links.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := Desired{
		Links: []Link{
			{Name: "ctag17", Kind: "vlan", Parent: "brtest", VLANID: 17},
			{Name: "brtest", Kind: "bridge", Alias: "bridge fixture", HardwareAddr: "02:00:00:77:00:01"},
			{Name: "vrftest", Kind: "vrf", VRFTable: 1007},
			{Name: "vxtest", Kind: "vxlan", Parent: "uplink0", VXLANID: 17007, Local: "198.51.100.10", Remote: "198.51.100.11"},
			{Name: "vmactest", Kind: "macvlan", Parent: "brtest", HardwareAddr: "00:00:5e:00:01:2a", MACVLANMode: "bridge", IPv6AddrGenMode: "none"},
		},
		Interfaces: []Interface{
			{Name: "brtest"}, {Name: "ctag17", Addresses: []string{"172.17.0.1/24"}, Master: stringPointer("vrftest")}, {Name: "vrftest"}, {Name: "vxtest"},
			{Name: "vmactest", Addresses: []string{"198.18.17.1/24", "fe80::200:5eff:fe00:12a/64", "2001:db8:77::1/64"}, AddressOptions: map[string]AddressOptions{"fe80::200:5eff:fe00:12a/64": {NoDAD: true}, "2001:db8:77::1/64": {NoDAD: true}}}, {Name: "member0", Master: stringPointer("brtest"), Alias: stringPointer("managed member")}, {Name: "uplink0", Master: stringPointer("brtest")},
		},
	}
	if _, err := e.Apply(context.Background(), 10, d); err != nil {
		t.Fatalf("native link creation: %v", err)
	}
	vmac, err := e.kernel.Interface("vmactest")
	if err != nil {
		t.Fatal(err)
	}
	if len(vmac.Addresses) != 3 || vmac.Link.IPv6AddrGenMode != "none" {
		t.Fatalf("HA link has unexpected automatic address: %+v", vmac)
	}
	for _, a := range vmac.Addresses {
		if strings.Contains(a.Address, ":") && (a.Flags&unix.IFA_F_NODAD == 0 || a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) != 0) {
			t.Fatalf("explicit HA VIP was not ready immediately: %+v", a)
		}
	}
	if _, err := e.Apply(context.Background(), 10, d); err != nil {
		t.Fatalf("native link reapply: %v", err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "10.77.0.3", "10.77.0.2")
	runNativeVRRPGuardFixture(t, e, d, clientNS, serverNS, client)
	d.Links[1].Alias = "updated bridge"
	d.Links[1].BridgeMulticastSnooping = boolPointer(false)
	d.Links[3].Remote = "198.51.100.12"
	d.Links[3].TTL = 32
	d.Links[3].Learning = boolPointer(false)
	d.Links[4].HardwareAddr = "00:00:5e:00:01:2b"
	if _, err := e.Apply(context.Background(), 11, d); err != nil {
		t.Fatalf("native link update: %v", err)
	}
	if _, err := e.Apply(context.Background(), 11, d); err != nil {
		t.Fatalf("native link update reapply: %v", err)
	}
	ctag, err := netlink.LinkByName("ctag17")
	if err != nil {
		t.Fatal(err)
	}
	foreign := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "foreign17", ParentIndex: ctag.Attrs().Index}, VlanId: 18, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}
	if err := netlink.LinkAdd(foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
		t.Fatal("native link removal deleted a foreign dependent link")
	}
	if _, err := netlink.LinkByName("foreign17"); err != nil {
		t.Fatalf("foreign child lost: %v", err)
	}
	if err := netlink.LinkDel(foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatalf("native link cleanup: %v", err)
	}
	for _, l := range d.Links {
		if _, err := netlink.LinkByName(l.Name); err == nil {
			t.Fatalf("owned link %s remains", l.Name)
		}
	}
	member, err = netlink.LinkByName("member0")
	if err != nil {
		t.Fatal(err)
	}
	if member.Attrs().Alias != "preexisting member" || member.Attrs().MasterIndex != 0 || member.Attrs().Flags&net.FlagUp != 0 {
		t.Fatalf("native member before-image not restored: %+v", member.Attrs())
	}
	t.Log("native bridge TCP, VLAN/VRF/VXLAN/macvlan create/update/reapply/remove and foreign-child preservation passed")
}

func runNativeVRRPGuardFixture(t *testing.T, e *Engine, base Desired, clientNS, serverNS netns.NsHandle, client *netlink.Handle) {
	t.Helper()
	peerAddress(t, client, "peerm0", "198.18.17.2/24")
	peerAddress(t, client, "peerm0", "2001:db8:77::2/64")
	routerNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer routerNS.Close()
	tcpExchange(t, clientNS, routerNS, "tcp4", "198.18.17.1", "198.18.17.2")
	tcpExchange(t, clientNS, routerNS, "tcp6", "2001:db8:77::1", "2001:db8:77::2")
	d := cloneDesired(base)
	d.VRRPGuards = []VRRPGuard{{Family: 4, Interface: "vmactest", Addresses: []string{"198.18.17.1"}}, {Family: 6, Interface: "vmactest", Addresses: []string{"2001:db8:77::1", "fe80::200:5eff:fe00:12a"}}}
	if _, err := e.Apply(context.Background(), 12, d); err != nil {
		t.Fatalf("install VRRP Accept_Mode guard: %v", err)
	}
	if _, err := e.Apply(context.Background(), 12, d); err != nil {
		t.Fatalf("reapply VRRP guard: %v", err)
	}
	for _, v := range []struct{ network, address string }{{"tcp4", "198.18.17.1"}, {"tcp6", "2001:db8:77::1"}} {
		assertLocalVIPConnectionDropped(t, clientNS, v.network, v.address)
	}
	assertGuardProtocolAllowed(t, clientNS, "ip4:112", "198.18.17.2", "198.18.17.1", []byte("vrrp-ipv4-guard-probe"), nil)
	assertGuardProtocolAllowed(t, clientNS, "ip6:112", "2001:db8:77::2", "2001:db8:77::1", []byte("vrrp-ipv6-guard-probe"), nil)
	peer, err := client.LinkByName("peerm0")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []byte{135, 136} {
		payload := make([]byte, 32)
		payload[0] = kind
		target := net.ParseIP("2001:db8:77::1").To16()
		if kind == 136 {
			payload[4] = 0x60
			target = net.ParseIP("2001:db8:77::2").To16()
		}
		copy(payload[8:24], target)
		payload[24] = 1
		if kind == 136 {
			payload[24] = 2
		}
		payload[25] = 1
		copy(payload[26:32], peer.Attrs().HardwareAddr)
		assertGuardProtocolAllowed(t, clientNS, "ip6:ipv6-icmp", "2001:db8:77::2", "2001:db8:77::1", payload, &kind)
	}
	// The HA table has no forward hook. Ordinary L2 traffic still crosses the
	// bridge while local acceptance of both VIP families is blocked.
	tcpExchange(t, clientNS, serverNS, "tcp4", "10.77.0.3", "10.77.0.2")
	if _, err := e.Apply(context.Background(), 10, base); err != nil {
		t.Fatalf("remove VRRP guard: %v", err)
	}
	tcpExchange(t, clientNS, routerNS, "tcp4", "198.18.17.1", "198.18.17.2")
	tcpExchange(t, clientNS, routerNS, "tcp6", "2001:db8:77::1", "2001:db8:77::2")
	guard, err := e.kernel.VRRPGuard()
	if err != nil || guard.Present {
		t.Fatalf("VRRP guard table remains: %+v %v", guard, err)
	}
	t.Log("VRRP Accept_Mode=false drops IPv4/IPv6 VIP TCP; NS/NA, protocol112 and bridge traffic remain allowed; removal restores VIP TCP")
}

func assertLocalVIPConnectionDropped(t *testing.T, clientNS netns.NsHandle, network, address string) {
	t.Helper()
	listener, err := net.Listen(network, net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	err = inNamespace(clientNS, func() error {
		dialer := net.Dialer{Timeout: 400 * time.Millisecond}
		conn, err := dialer.Dial(network, listener.Addr().String())
		if conn != nil {
			conn.Close()
		}
		return err
	})
	var timeout net.Error
	if err == nil || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("VIP connection was not dropped by input guard: %v", err)
	}
}

func assertGuardProtocolAllowed(t *testing.T, clientNS netns.NsHandle, network, source, destination string, payload []byte, icmpType *byte) {
	t.Helper()
	listener, err := net.ListenPacket(network, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var sender net.PacketConn
	if err := inNamespace(clientNS, func() error { var err error; sender, err = net.ListenPacket(network, source); return err }); err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if icmpType != nil {
		if err := ipv6.NewPacketConn(sender).SetHopLimit(255); err != nil {
			t.Fatal(err)
		}
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.WriteTo(payload, &net.IPAddr{IP: net.ParseIP(destination)}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	for {
		n, _, err := listener.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("required protocol exception %s to VIP %s failed: %v", network, destination, err)
		}
		if icmpType != nil {
			if n >= 24 && buffer[0] == *icmpType && bytes.Equal(buffer[8:24], payload[8:24]) {
				return
			}
		} else if bytes.Contains(buffer[:n], payload) {
			return
		}
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

func tcpExchange(t *testing.T, client, server netns.NsHandle, network, address string, wantSources ...string) string {
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
	type exchangeResult struct {
		source string
		err    error
	}
	result := make(chan exchangeResult, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- exchangeResult{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		source, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		accepted := false
		for _, want := range wantSources {
			accepted = accepted || source == want
		}
		if err != nil || !accepted {
			result <- exchangeResult{source, fmt.Errorf("source=%s want=%v err=%v", source, wantSources, err)}
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			result <- exchangeResult{source, err}
			return
		}
		_, err = conn.Write(payload)
		result <- exchangeResult{source, err}
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
	observed := <-result
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	return observed.source
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
