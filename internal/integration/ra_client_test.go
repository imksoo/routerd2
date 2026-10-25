//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
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
	for key := range prior {
		value, err := os.ReadFile("/proc/sys/net/ipv6/conf/wan0/" + key)
		if err != nil || strings.TrimSpace(string(value)) != "0" {
			t.Fatalf("native RA must own %s before the first packet: %s %v", key, value, err)
		}
	}
	packet := make([]byte, 16+32+8+24)
	packet[0], packet[4], packet[5] = 134, 64, 0x08 // RA, current hop limit, high preference
	binary.BigEndian.PutUint16(packet[6:8], 8)
	pio := packet[16:48]
	pio[0], pio[1], pio[2], pio[3] = 3, 4, 64, 0xc0
	binary.BigEndian.PutUint32(pio[4:8], 8)
	binary.BigEndian.PutUint32(pio[8:12], 6)
	prefix := netip.MustParseAddr("2001:db8:10::").As16()
	copy(pio[16:], prefix[:])
	packet[48], packet[49] = 1, 1
	copy(packet[50:56], peer.Attrs().HardwareAddr)
	dns := packet[56:]
	dns[0], dns[1] = 25, 3
	binary.BigEndian.PutUint32(dns[4:8], 5)
	resolver := netip.MustParseAddr("2001:db8:53::53").As16()
	copy(dns[8:], resolver[:])
	if err := unix.Sendto(fd, packet, 0, &unix.SockaddrInet6{Addr: netip.MustParseAddr("ff02::1").As16(), ZoneId: uint32(peer.Attrs().Index)}); err != nil {
		t.Fatal(err)
	}
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
			if value.IPNet.String() == want {
				return true
			}
		}
		return false
	}
	until(t, 5*time.Second, func() bool {
		return hasAddress("2001:db8:10::1234/128") && fixtureRoute(t, "::/0", "fe80::2", link.Attrs().Index)
	})
	var state runtimeplan.InstanceStatus
	if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan-ra", nil, &state); err != nil {
		t.Fatal(err)
	}
	var snapshot raclient.Snapshot
	if err := json.Unmarshal(state.Snapshot, &snapshot); err != nil || !snapshot.Active || snapshot.Received != 1 || len(snapshot.Addresses) != 1 || len(snapshot.DNSServers) != 1 {
		t.Fatalf("wire RA did not reach the typed service state: %+v %v", snapshot, err)
	}
	var status supervisor.Status
	if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	pids := []int{f.pid}
	for _, module := range status.Modules {
		if module.Name == "control" || module.Name == "service.wan-ra" {
			pids = append(pids, module.PID)
		}
	}
	if len(pids) != 3 {
		t.Fatal("required process ownership is missing", status)
	}
	var stopped []int
	resume := func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			_ = syscall.Kill(stopped[i], syscall.SIGCONT)
		}
		stopped = nil
	}
	t.Cleanup(resume)
	// Suspend supervision too so heartbeat recovery cannot masquerade as
	// autonomous kernel expiry. The processes remain alive throughout.
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		stopped = append(stopped, pid)
	}
	until(t, 10*time.Second, func() bool {
		_, routeErr := netlink.RouteGet(netip.MustParseAddr("2001:db8:ffff::1").AsSlice())
		return routeErr != nil && !hasAddress("2001:db8:10::1234/128")
	})
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d exited before autonomous kernel expiry: %v", pid, err)
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
