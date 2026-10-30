//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCPRelayVRFIsolation(t *testing.T) {
	if !isolateFixture(t, "TestDaemonDHCPRelayVRFIsolation") {
		return
	}
	// This namespace-local setting permits an unbound UDP socket to receive
	// VRF traffic when its VRF-bound peer listener has disappeared.
	if err := os.WriteFile("/proc/sys/net/ipv4/udp_l3mdev_accept", []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	domains := []struct{ name, vrf, response string }{{"blue", "blue", "B"}, {"red", "red", "R"}, {"default", "", "D"}}
	var clients []netns.NsHandle
	var servers []*os.Process
	var captures [][2]*healthPBRCaptureState
	doc := config.Native{}
	forward := true
	doc.Network.Forwarding.IPv4 = &forward
	for i, domain := range domains {
		wan, lan := fmt.Sprintf("wan%d", i), fmt.Sprintf("lan%d", i)
		provider := fixturePeerNamespace(t, wan, "server0")
		clients = append(clients, fixturePeerNamespace(t, lan, "client0"))
		if err := fixtureInsideNamespace(provider, func() error {
			link, err := netlink.LinkByName("server0")
			if err != nil {
				return err
			}
			address, _ := netlink.ParseAddr("198.51.100.2/24")
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
			_, prefix, _ := net.ParseCIDR("192.0.2.0/24")
			return netlink.RouteAdd(&netlink.Route{Dst: prefix, Gw: net.ParseIP("198.51.100.1"), LinkIndex: link.Attrs().Index})
		}); err != nil {
			t.Fatal(err)
		}
		var master *string
		if domain.vrf != "" {
			name := domain.vrf
			master = &name
			doc.Network.Links = append(doc.Network.Links, network.Link{Name: name, Kind: "vrf", VRFTable: 1001 + i})
			doc.Network.Interfaces = append(doc.Network.Interfaces, network.Interface{Name: name})
		}
		doc.Network.Interfaces = append(doc.Network.Interfaces,
			network.Interface{Name: wan, Master: master, Addresses: []string{"198.51.100.1/24"}},
			network.Interface{Name: lan, Master: master, Addresses: []string{"192.0.2.1/24"}})
		doc.Services = append(doc.Services, serviceconfig.Service{Name: domain.name + "-relay", DHCPRelay: &dhcprelayconfig.Config{VRF: domain.vrf, Interfaces: []dhcprelayconfig.Interface{{Name: lan, Address: "192.0.2.1", Servers: []string{"198.51.100.2"}}}}})
		server := startFixtureDNSMasq(t, provider, "server0", []string{"--port=0", "--dhcp-range=192.0.2.100,192.0.2.110,255.255.255.0,60s", "--dhcp-option=3,192.0.2.1", "--dhcp-option=15," + domain.name + ".test"})
		servers = append(servers, server.Process)
		vrrpVIPResponders(t, provider, "4", []string{"198.51.100.2"}, domain.response)
	}
	f := startFixtureDaemon(t, config.FromNative(doc))
	for i := range domains {
		captures = append(captures, [2]*healthPBRCaptureState{healthPBRCapture(t, fmt.Sprintf("wan%d", i)), healthPBRCapture(t, fmt.Sprintf("lan%d", i))})
	}
	module := func(name string) (supervisor.ModuleStatus, bool) {
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, worker := range status.Modules {
			if worker.Name == "service."+name+"-relay" {
				return worker, true
			}
		}
		return supervisor.ModuleStatus{}, false
	}
	var leases []fixtureDHCPRelayLease
	var workers []supervisor.ModuleStatus
	acquire := func(i int) fixtureDHCPRelayLease {
		t.Helper()
		lease := acquireFixtureDHCPRelayLease(t, clients[i], "client0", false, "")
		if lease.Server != "198.51.100.2" || lease.Router != "192.0.2.1" || lease.Domain != domains[i].name+".test" {
			t.Fatal("DHCP reply crossed routing domains", domains[i].name, lease)
		}
		return lease
	}
	traffic := func(i int) {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			port := "18093"
			if protocol == "udp4" {
				port = "18094"
			}
			var conn net.Conn
			if err := fixtureInsideNamespace(clients[i], func() error {
				var err error
				conn, err = net.DialTimeout(protocol, "198.51.100.2:"+port, time.Second)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			_, writeErr := conn.Write([]byte("request"))
			var response [1]byte
			_, readErr := io.ReadFull(conn, response[:])
			conn.Close()
			if writeErr != nil || readErr != nil || string(response[:]) != domains[i].response {
				t.Fatal("client traffic crossed routing domains", domains[i].name, protocol, writeErr, readErr, response)
			}
		}
	}
	for i, domain := range domains {
		lease := acquire(i)
		leases = append(leases, lease)
		if err := fixtureInsideNamespace(clients[i], func() error {
			link, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			address, _ := netlink.ParseAddr(lease.Address + "/24")
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
			return netlink.RouteAdd(&netlink.Route{Gw: net.ParseIP("192.0.2.1"), LinkIndex: link.Attrs().Index})
		}); err != nil {
			t.Fatal(err)
		}
		worker, found := module(domain.name)
		if !found || !worker.Ready || worker.PID <= 1 {
			t.Fatal("relay worker is not running", domain.name, worker)
		}
		workers = append(workers, worker)
		traffic(i)
	}
	// Queue a genuine server reply, then retire just its relay. The identical
	// default-domain giaddr/server must not make that old reply valid there.
	if err := servers[0].Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	until(t, time.Second, func() bool {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", servers[0].Pid))
		return err == nil && strings.Contains(string(status), "State:\tT")
	})
	const xid = 0x5b6c7d8e
	if err := fixtureInsideNamespace(clients[0], func() error {
		iface, err := net.InterfaceByName("client0")
		if err != nil {
			return err
		}
		listen := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
				if inner == nil {
					inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "client0")
				}
			})
			if err != nil {
				return err
			}
			return inner
		}}
		socket, err := listen.ListenPacket(context.Background(), "udp4", "0.0.0.0:68")
		if err != nil {
			return err
		}
		defer socket.Close()
		request := make([]byte, 300)
		request[0], request[1], request[2] = 1, 1, 6
		binary.BigEndian.PutUint32(request[4:8], xid)
		binary.BigEndian.PutUint16(request[10:12], 0x8000)
		copy(request[12:16], net.ParseIP(leases[0].Address).To4())
		copy(request[28:34], iface.HardwareAddr)
		copy(request[236:], []byte{99, 130, 83, 99, 53, 1, 8, 255})
		_, err = socket.WriteTo(request, &net.UDPAddr{IP: net.IPv4bcast, Port: 67})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	until(t, 2*time.Second, func() bool { return relayFixtureXIDCount(captures[0][0], xid, 1) > 0 })
	remaining := doc
	remaining.Services = append([]serviceconfig.Service(nil), doc.Services[1:]...)
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-blue-relay", Config: config.FromNative(remaining)}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool { _, found := module("blue"); return !found })
	if err := servers[0].Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, 2*time.Second, func() bool { return relayFixtureXIDCount(captures[0][0], xid, 2) > 0 })
	for _, i := range []int{2, 1} {
		acquire(i)
		traffic(i)
		worker, found := module(domains[i].name)
		if !found || worker.PID != workers[i].PID || worker.Generation != workers[i].Generation {
			t.Fatal("unrelated relay restarted", domains[i].name, worker)
		}
	}
	for i, domain := range domains {
		if relayFixtureXIDCount(captures[i][1], xid, 2) != 0 {
			t.Fatal("retired blue relay reply leaked onto a client LAN", domain.name)
		}
	}
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 2, RequestID: "restore-blue-relay", Config: config.FromNative(doc)}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		worker, found := module("blue")
		return found && worker.Ready && worker.Generation > workers[0].Generation && worker.ConfigDigest == workers[0].ConfigDigest
	})
	acquire(0)
	traffic(0)
	t.Log("independent blue/red/default DHCP and TCP/UDP; identical endpoints, late reply after retirement, unchanged other workers and blue recovery passed")
}

func relayFixtureXIDCount(capture *healthPBRCaptureState, xid uint32, op byte) int {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	count := 0
	for _, packet := range capture.packets {
		b := packet.data
		if len(b) < 278 || b[12] != 8 || b[13] != 0 || b[23] != 17 {
			continue
		}
		body := 14 + int(b[14]&15)*4 + 8
		if body+8 <= len(b) && b[body] == op && binary.BigEndian.Uint32(b[body+4:body+8]) == xid {
			count++
		}
	}
	return count
}
