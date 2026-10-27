//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/clocktest"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// This gate runs inside the dedicated no-NIC QEMU image when the host cannot
// provide namespace-local writable procfs. Only the verified fresh network
// namespace is ever changed. It exercises the actual two-ELF process tree.
func TestDaemonRAClientConfigurationAndExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonRAClientConfigurationAndExpiry") {
		return
	}
	runRAClientFixture(t, nil)
}

func TestDaemonRAClientClockCorrections(t *testing.T) {
	file, err := clocktest.ForwardFileFromEnv()
	if err != nil {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("dedicated no-NIC QEMU clock broker required")
	}
	file.Close()
	if !isolateFixtureTimeout(t, "TestDaemonRAClientClockCorrections", 120*time.Second) {
		return
	}
	client, err := clocktest.ClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.Step("restore"); err != nil {
			t.Error(err)
		}
		client.Close()
	})
	runRAClientFixture(t, client)
}

func runRAClientFixture(t *testing.T, clockClient *clocktest.Client) {
	peerNS := fixturePeerNamespace(t, "wan0", "provider0")
	h, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	peer, err := h.LinkByName("provider0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("fe80::2/64")
	address.Flags = unix.IFA_F_NODAD
	if err := h.AddrReplace(peer, address); err != nil {
		t.Fatal(err)
	}
	loopback, err := h.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := netlink.ParseAddr("2001:db8:ffff::9/128")
	backend.Flags = unix.IFA_F_NODAD
	if err := h.AddrAdd(loopback, backend); err != nil {
		t.Fatal(err)
	}
	_, subnet, _ := net.ParseCIDR("2001:db8:10::/64")
	if err := h.RouteAdd(&netlink.Route{Dst: subnet, LinkIndex: peer.Attrs().Index, Scope: netlink.SCOPE_LINK}); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, peerNS, "6", []string{"2001:db8:ffff::9"}, "R")
	capture := healthPBRCapture(t, "wan0")
	traffic := func() {
		t.Helper()
		for _, protocol := range []string{"tcp6", "udp6"} {
			port := "18093"
			dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("2001:db8:10::1234")}}
			if protocol == "udp6" {
				port, dialer.LocalAddr = "18094", &net.UDPAddr{IP: net.ParseIP("2001:db8:10::1234")}
			}
			conn, err := dialer.Dial(protocol, net.JoinHostPort("2001:db8:ffff::9", port))
			if err != nil {
				t.Fatal(protocol, err)
			}
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if protocol == "udp6" {
				_, err = conn.Write([]byte("RA"))
			}
			var response [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, response[:])
			}
			conn.Close()
			if err != nil || response[0] != 'R' {
				t.Fatal("RA-learned source/default route TCP/UDP", protocol, response, err)
			}
		}
	}
	prior := map[string]string{}
	for _, key := range []string{"accept_ra", "autoconf"} {
		value, err := os.ReadFile("/proc/sys/net/ipv6/conf/wan0/" + key)
		if err != nil {
			t.Fatal(err)
		}
		prior[key] = strings.TrimSpace(string(value))
	}
	fd := -1
	err = fixtureInsideNamespace(peerNS, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
		if err != nil {
			return err
		}
		for option, value := range map[int]int{unix.IPV6_MULTICAST_IF: peer.Attrs().Index, unix.IPV6_MULTICAST_HOPS: 255, unix.IPV6_UNICAST_HOPS: 255} {
			if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, option, value); err != nil {
				return err
			}
		}
		return unix.Bind(fd, &unix.SockaddrInet6{Addr: netip.MustParseAddr("fe80::2").As16(), ZoneId: uint32(peer.Attrs().Index)})
	})
	if err != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		t.Fatal(err)
	}
	defer unix.Close(fd)
	doc := config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"fe80::1/64"}, AddressOptions: map[string]network.AddressOptions{"fe80::1/64": {NoDAD: true}}}}},
		Services: []serviceconfig.Service{{Name: "wan-ra", RAClient: &serviceconfig.RAClient{Interface: "wan0", SourceAddress: "fe80::1", InterfaceID: "::1234", AllowedRouters: []string{"fe80::2"}}}},
	}
	f := startFixtureDaemon(t, config.FromNative(doc))
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == name {
				return module
			}
		}
		return supervisor.ModuleStatus{}
	}
	for key := range prior {
		value, err := os.ReadFile("/proc/sys/net/ipv6/conf/wan0/" + key)
		if err != nil || strings.TrimSpace(string(value)) != "0" {
			t.Fatalf("native RA must own %s before the first packet: %s %v", key, value, err)
		}
	}
	packet := make([]byte, 16+32+8+24)
	packet[0], packet[4], packet[5] = 134, 64, 0x08 // RA, current hop limit, high preference
	valid, preferred := uint32(8), uint32(6)
	if clockClient != nil {
		valid, preferred = 20, 18
	}
	binary.BigEndian.PutUint16(packet[6:8], uint16(valid))
	pio := packet[16:48]
	pio[0], pio[1], pio[2], pio[3] = 3, 4, 64, 0xc0
	binary.BigEndian.PutUint32(pio[4:8], valid)
	binary.BigEndian.PutUint32(pio[8:12], preferred)
	prefix := netip.MustParseAddr("2001:db8:10::").As16()
	copy(pio[16:], prefix[:])
	packet[48], packet[49] = 1, 1
	copy(packet[50:56], peer.Attrs().HardwareAddr)
	dns := packet[56:]
	dns[0], dns[1] = 25, 3
	binary.BigEndian.PutUint32(dns[4:8], valid-2)
	resolver := netip.MustParseAddr("2001:db8:53::53").As16()
	copy(dns[8:], resolver[:])
	// Broken independent DNS options must not discard this valid PIO/router.
	// Both options fit the packet; only their own contents are unusable.
	packet = append(packet, 25, 1, 0, 0, 0, 0, 0, 5)
	packet = append(packet, 31, 2, 0, 0, 0, 0, 0, 5, 0xc0, 0, 0, 0, 0, 0, 0, 0)
	// Each option has valid framing, so its unusable contents do not invalidate
	// the usable router, PIO or subsequent RIOs. Repeated RIOs update in order.
	packet = append(packet, 3, 1, 0, 0, 0, 0, 0, 0)
	packet = append(packet, 5, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	packet = append(packet, 24, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	packet = append(packet, 24, 1, 0, 8, 0, 0, 0, byte(valid))
	packet = append(packet, 24, 1, 0, 0, 0, 0, 0, byte(valid))
	initialSize := len(packet)
	// Full DNS repositories must not discard this router's lifetime refresh.
	extraDNS := make([]byte, 8+16*40)
	extraDNS[0], extraDNS[1] = 25, byte(len(extraDNS)/8)
	binary.BigEndian.PutUint32(extraDNS[4:8], valid-3)
	for i := 0; i < 40; i++ {
		a := netip.MustParseAddr(fmt.Sprintf("2001:db8:53::%x", i+256)).As16()
		copy(extraDNS[8+16*i:], a[:])
	}
	packet = append(packet, extraDNS...)
	search := make([]byte, 8)
	search[0] = 31
	binary.BigEndian.PutUint32(search[4:8], valid-3)
	for i := 0; i < 40; i++ {
		label := fmt.Sprintf("d%d", i)
		search = append(search, byte(len(label)))
		search = append(search, label...)
		search = append(search, 4, 't', 'e', 's', 't', 0)
	}
	for len(search)%8 != 0 {
		search = append(search, 0)
	}
	search[1] = byte(len(search) / 8)
	packet = append(packet, search...)
	send := func() {
		t.Helper()
		if err := unix.Sendto(fd, packet, 0, &unix.SockaddrInet6{Addr: netip.MustParseAddr("ff02::1").As16(), ZoneId: uint32(peer.Attrs().Index)}); err != nil {
			t.Fatal(err)
		}
	}
	refresh := packet
	packet = append([]byte(nil), packet[:initialSize]...)
	binary.BigEndian.PutUint16(packet[6:8], 1)
	binary.BigEndian.PutUint32(packet[len(packet)-4:], 1)
	send()
	var first raclient.Snapshot
	until(t, 3*time.Second, func() bool {
		var initial runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan-ra", nil, &initial); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(initial.Snapshot, &first); err != nil {
			t.Fatal(err)
		}
		return first.Received == 1 && len(first.Routes) == 1
	})
	packet = refresh
	send()
	link, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	hasAddress := func(want string) bool {
		values, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			if value.IPNet.String() == want && value.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				return true
			}
		}
		return false
	}
	until(t, 5*time.Second, func() bool {
		return hasAddress("2001:db8:10::1234/128") && fixtureRoute(t, "::/0", "fe80::2", link.Attrs().Index)
	})
	traffic()
	var state runtimeplan.InstanceStatus
	if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan-ra", nil, &state); err != nil {
		t.Fatal(err)
	}
	var snapshot raclient.Snapshot
	if err := json.Unmarshal(state.Snapshot, &snapshot); err != nil || !snapshot.Active || snapshot.Received != 2 || len(snapshot.Addresses) != 1 || len(snapshot.DNSServers) != raclient.MaxDNSServers || len(snapshot.SearchDomains) != raclient.MaxSearchDomains {
		t.Fatalf("wire RA did not reach the typed service state: %+v %v", snapshot, err)
	}
	if len(snapshot.Routes) != 1 || snapshot.Routes[0].ValidUntil.Nanos <= first.Routes[0].ValidUntil.Nanos {
		t.Fatal("DNS capacity prevented default-route lifetime refresh", first.Routes, snapshot.Routes)
	}
	retainedResolver := false
	for _, resolver := range snapshot.DNSServers {
		retainedResolver = retainedResolver || resolver.Address == "2001:db8:53::53"
	}
	if !retainedResolver {
		t.Fatal("full DNS repository evicted the later-expiring resolver", snapshot.DNSServers)
	}
	if clockClient != nil {
		before := module("service.wan-ra")
		if before.PID < 2 {
			t.Fatal("RA worker missing")
		}
		if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 5*time.Second, func() bool {
			next := module("service.wan-ra")
			return next.Ready && next.PID > 1 && next.PID != before.PID
		})
		until(t, 5*time.Second, func() bool {
			if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan-ra", nil, &state); err != nil {
				t.Fatal(err)
			}
			var restored raclient.Snapshot
			if err := json.Unmarshal(state.Snapshot, &restored); err != nil {
				t.Fatal(err)
			}
			return len(restored.Addresses) == 1 && restored.Addresses[0].ValidUntil == snapshot.Addresses[0].ValidUntil && restored.Routes[0].ValidUntil == snapshot.Routes[0].ValidUntil
		})
		traffic()
	}
	pids := []int{f.pid, module("control").PID, module("service.wan-ra").PID, module("network").PID}
	for _, pid := range pids {
		if pid < 2 {
			t.Fatal("required process ownership missing", pids)
		}
	}
	var stopped []int
	resume := func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			_ = syscall.Kill(stopped[i], syscall.SIGCONT)
		}
		stopped = nil
	}
	t.Cleanup(resume)
	stop := func(pid int) {
		t.Helper()
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		stopped = append(stopped, pid)
	}
	if clockClient != nil {
		stop(pids[2])
		for _, action := range []string{"forward", "backward"} {
			result, err := clockClient.Step(action)
			if err != nil {
				t.Fatal(err)
			}
			traffic()
			var current runtimeplan.InstanceStatus
			if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan-ra", nil, &current); err != nil {
				t.Fatal(err)
			}
			if current.Sequence != state.Sequence || !bytes.Equal(current.Snapshot, state.Snapshot) {
				t.Fatal("clock step/poll rewrote a frozen RA observation")
			}
			t.Log("actual guest UTC correction", action, result.Clock.WallTime, result.Clock.BoottimeNanoseconds)
		}
		writer := module("network")
		if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 5*time.Second, func() bool { next := module("network"); return next.Ready && next.PID > 1 && next.PID != writer.PID })
		pids[3] = module("network").PID
		traffic()
		stop(pids[0])
		stop(pids[1])
		stop(pids[3])
	} else {
		for _, pid := range pids {
			stop(pid)
		}
	}
	// With both owners and supervision frozen, only kernel timers can expire
	// the learned /128 and default route. Host UTC is never changed.
	until(t, time.Duration(valid+2)*time.Second, func() bool {
		_, routeErr := netlink.RouteGet(netip.MustParseAddr("2001:db8:ffff::1").AsSlice())
		return routeErr != nil && !hasAddress("2001:db8:10::1234/128")
	})
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d exited before autonomous kernel expiry: %v", pid, err)
		}
	}
	if clockClient != nil {
		if _, err := clockClient.Step("restore"); err != nil {
			t.Fatal(err)
		}
	}
	resume()
	until(t, 12*time.Second, func() bool {
		var recovered supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &recovered); err != nil || !recovered.Ready {
			return false
		}
		var s control.State
		return f.control.Do(context.Background(), "GET", "/v1/config", nil, &s) == nil && s.Healthy
	})
	if clockClient != nil {
		replay := healthPBRUpdate(state)
		replay.Sequence++
		if err := f.control.Do(context.Background(), "POST", "/v1/services/state", replay, nil); err != nil && !localapi.IsHTTPStatus(err, 409) {
			t.Fatal(err)
		}
		if hasAddress("2001:db8:10::1234/128") || fixtureRoute(t, "::/0", "fe80::2", link.Attrs().Index) {
			t.Fatal("expired RA replay restored kernel authority")
		}
		send()
		until(t, 5*time.Second, func() bool {
			return hasAddress("2001:db8:10::1234/128") && fixtureRoute(t, "::/0", "fe80::2", link.Attrs().Index)
		})
		traffic()
		t.Log("RA and network worker restart, actual UTC +/-24h, all owners stopped, kernel expiry, expired replay rejection and fresh RA TCP/UDP recovery")
	}
	until(t, time.Second, func() bool {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		counts := map[string]int{}
		for _, p := range capture.packets {
			b := p.data
			if len(b) < 58 || binary.BigEndian.Uint16(b[12:14]) != unix.ETH_P_IPV6 {
				continue
			}
			if b[20] == unix.IPPROTO_ICMPV6 && b[54] == 134 && b[21] == 255 {
				counts["ra"]++
				continue
			}
			if b[20] != unix.IPPROTO_TCP && b[20] != unix.IPPROTO_UDP {
				continue
			}
			source, dest := net.IP(b[22:38]), net.IP(b[38:54])
			if source.Equal(net.ParseIP("2001:db8:10::1234")) && dest.Equal(net.ParseIP("2001:db8:ffff::9")) {
				counts[fmt.Sprintf("%d-out", b[20])]++
			}
			if dest.Equal(net.ParseIP("2001:db8:10::1234")) && source.Equal(net.ParseIP("2001:db8:ffff::9")) {
				counts[fmt.Sprintf("%d-in", b[20])]++
			}
		}
		return counts["ra"] > 0 && counts["6-out"] > 0 && counts["6-in"] > 0 && counts["17-out"] > 0 && counts["17-in"] > 0
	})
	doc.Services = nil
	var committed control.State
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-ra-client", Config: config.FromNative(doc)}, &committed); err != nil {
		t.Fatal(err)
	}
	for key, want := range prior {
		value, err := os.ReadFile("/proc/sys/net/ipv6/conf/wan0/" + key)
		if err != nil || strings.TrimSpace(string(value)) != want {
			t.Fatalf("removing RA worker did not restore %s: got %s, want %s: %v", key, value, want, err)
		}
	}
	if !hasAddress("fe80::1/64") {
		t.Fatal("RA worker cleanup removed the separately owned static link-local address")
	}
	t.Log("actual RA worker -> typed coordinator -> finite kernel address/route; autonomous expiry with control/supervisor stopped; configuration removal restored RA sysctls")
}
