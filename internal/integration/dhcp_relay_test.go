//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCPRelayIndependentClientsAndServer(t *testing.T) {
	if !isolateFixture(t, "TestDaemonDHCPRelayIndependentClientsAndServer") {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "server0")
	clients := []netns.NsHandle{fixturePeerNamespace(t, "lan0", "client0"), fixturePeerNamespace(t, "lan1", "client1")}
	if err := fixtureInsideNamespace(provider, func() error {
		link, err := netlink.LinkByName("server0")
		if err != nil {
			return err
		}
		address, _ := netlink.ParseAddr("198.51.100.2/24")
		if err := netlink.AddrAdd(link, address); err != nil {
			return err
		}
		for _, prefix := range []string{"192.0.2.0/24", "203.0.113.0/24"} {
			_, destination, _ := net.ParseCIDR(prefix)
			if err := netlink.RouteAdd(&netlink.Route{Dst: destination, Gw: net.ParseIP("198.51.100.1"), LinkIndex: link.Attrs().Index}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	forward := true
	relay := dhcprelayconfig.Config{Interfaces: []dhcprelayconfig.Interface{
		{Name: "lan0", Address: "192.0.2.1", Servers: []string{"198.51.100.2"}},
		{Name: "lan1", Address: "203.0.113.1", Servers: []string{"198.51.100.2"}},
	}}
	f := startFixtureDaemon(t, config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{
		{Name: "wan0", Addresses: []string{"198.51.100.1/24"}},
		{Name: "lan0", MTU: 600, Addresses: []string{"192.0.2.1/24"}},
		{Name: "lan1", Addresses: []string{"203.0.113.1/24"}},
	}, Forwarding: network.Forwarding{IPv4: &forward}}, Services: []serviceconfig.Service{{Name: "relay", DHCPRelay: &relay}}}))
	captures := []*healthPBRCaptureState{healthPBRCapture(t, "wan0"), healthPBRCapture(t, "lan0"), healthPBRCapture(t, "lan1")}
	startFixtureDNSMasq(t, provider, []string{"--dhcp-range=set:lan0,192.0.2.100,192.0.2.110,255.255.255.0,60s", "--dhcp-option=tag:lan0,3,192.0.2.1",
		"--dhcp-range=set:lan1,203.0.113.100,203.0.113.110,255.255.255.0,60s", "--dhcp-option=tag:lan1,3,203.0.113.1",
		"--dhcp-vendorclass=set:large,relay-large", "--dhcp-option-force=tag:large,224," + strings.Repeat("A", 200), "--dhcp-option-force=tag:large,225," + strings.Repeat("B", 200)})
	acquire := func(i int, requested string) string {
		t.Helper()
		lease := acquireFixtureDHCPRelayLease(t, clients[i], fmt.Sprintf("client%d", i), i == 1, requested)
		prefix := []string{"192.0.2.0/24", "203.0.113.0/24"}[i]
		if !netip.MustParsePrefix(prefix).Contains(netip.MustParseAddr(lease.Address)) || lease.Server != "198.51.100.2" || lease.Router != relay.Interfaces[i].Address {
			t.Fatal("wrong client subnet, upstream server or gateway", lease)
		}
		return lease.Address
	}
	vrrpVIPResponders(t, provider, "4", []string{"198.51.100.2"}, "D")
	var leases []string
	for i := range clients {
		address := acquire(i, "")
		leases = append(leases, address)
		if err := fixtureInsideNamespace(clients[i], func() error {
			link, err := netlink.LinkByName(fmt.Sprintf("client%d", i))
			if err != nil {
				return err
			}
			ip, _ := netlink.ParseAddr(address + "/24")
			if err := netlink.AddrAdd(link, ip); err != nil {
				return err
			}
			return netlink.RouteAdd(&netlink.Route{Gw: net.ParseIP(relay.Interfaces[i].Address), LinkIndex: link.Attrs().Index})
		}); err != nil {
			t.Fatal(err)
		}
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
			if writeErr != nil || readErr != nil || response[0] != 'D' {
				t.Fatal("leased client traffic", protocol, writeErr, readErr, response)
			}
		}
	}
	// An independent DHCPINFORM uses ciaddr without allocating another lease.
	// The hop limit must discard a looped request before the valid exchange.
	var socket net.PacketConn
	var clientMAC net.HardwareAddr
	if err := fixtureInsideNamespace(clients[0], func() error {
		iface, err := net.InterfaceByName("client0")
		if err != nil {
			return err
		}
		clientMAC = iface.HardwareAddr
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
		socket, err = listen.ListenPacket(context.Background(), "udp4", "0.0.0.0:68")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	conn := ipv4.NewPacketConn(socket)
	if err := conn.SetControlMessage(ipv4.FlagDst, true); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, 300)
	request[0], request[1], request[2], request[3] = 1, 1, 6, 5
	binary.BigEndian.PutUint32(request[4:8], 0x71829304)
	copy(request[12:16], net.ParseIP(leases[0]).To4())
	copy(request[28:34], clientMAC)
	options := []byte{99, 130, 83, 99, 53, 1, 8, 55, 2, 224, 225, 57, 2, 0x10, 0, 60, 11}
	options = append(options, []byte("relay-large")...)
	options = append(options, 255)
	copy(request[236:], options)
	send := func() {
		t.Helper()
		if _, err := conn.WriteTo(request, nil, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
			t.Fatal(err)
		}
	}
	send()
	conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	response := make([]byte, 4096)
	if _, _, _, err := conn.ReadFrom(response); err == nil {
		t.Fatal("request beyond the hop limit reached the DHCP server")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
	request[3] = 0
	for _, broadcast := range []bool{false, true} {
		wantDestination := leases[0]
		xid := uint32(0x71829304)
		if broadcast {
			wantDestination = "255.255.255.255"
			xid++
			binary.BigEndian.PutUint16(request[10:12], 0x8000)
		}
		binary.BigEndian.PutUint32(request[4:8], xid)
		send()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, metadata, _, err := conn.ReadFrom(response)
		if err != nil || n < 240 || metadata == nil || metadata.Dst.String() != wantDestination || response[0] != 2 || binary.BigEndian.Uint32(response[4:8]) != xid || net.IP(response[16:20]).String() != "0.0.0.0" {
			t.Fatal("DHCPINFORM requires an ACK to ciaddr with zero yiaddr", n, metadata, err)
		}
		if n <= 600 {
			t.Fatal("large DHCPINFORM reply did not exceed the client link MTU", n)
		}
		large := map[byte]string{}
		for at := 240; at < n; {
			code := response[at]
			at++
			if code == 255 {
				break
			}
			if code == 0 {
				continue
			}
			if at >= n || int(response[at])+at+1 > n {
				t.Fatal("truncated reassembled DHCP options")
			}
			length := int(response[at])
			at++
			if code == 224 || code == 225 {
				large[code] += string(response[at : at+length])
			}
			at += length
		}
		if large[224] != strings.Repeat("A", 200) || large[225] != strings.Repeat("B", 200) {
			t.Fatal("large DHCP options changed in transit", len(large[224]), len(large[225]))
		}
	}
	module := func() supervisor.ModuleStatus {
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, worker := range status.Modules {
			if worker.Name == "service.relay" {
				return worker
			}
		}
		return supervisor.ModuleStatus{}
	}
	before := module()
	if !before.Ready || before.PID <= 1 {
		t.Fatal("relay worker is not running", before)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 8*time.Second, func() bool {
		worker := module()
		return worker.Ready && worker.Generation > before.Generation
	})
	acquire(0, leases[0])
	until(t, time.Second, func() bool {
		fragments := map[string]byte{}
		captures[1].mu.Lock()
		for _, packet := range captures[1].packets {
			b := packet.data
			if len(b) < 34 || b[12] != 8 || b[13] != 0 || b[23] != 17 || net.IP(b[26:30]).String() != "198.51.100.2" {
				continue
			}
			flags := binary.BigEndian.Uint16(b[20:22])
			if flags&0x3fff == 0 {
				continue
			}
			if binary.BigEndian.Uint16(b[16:18]) > 600 {
				captures[1].mu.Unlock()
				t.Fatal("fragment exceeds client link MTU")
			}
			destination := net.IP(b[30:34]).String()
			if flags&0x1fff == 0 && flags&0x2000 != 0 {
				fragments[destination] |= 1
			}
			if flags&0x1fff != 0 && flags&0x2000 == 0 {
				fragments[destination] |= 2
			}
		}
		captures[1].mu.Unlock()
		if fragments[leases[0]] != 3 || fragments["255.255.255.255"] != 3 {
			return false
		}
		for i, iface := range relay.Interfaces {
			if captures[0].count(iface.Address, "198.51.100.2", 17, 67) == 0 {
				return false
			}
			captures[0].mu.Lock()
			forwarded := false
			for _, packet := range captures[0].packets {
				b := packet.data
				if len(b) < 278 || b[12] != 8 || b[13] != 0 || b[23] != 17 {
					continue
				}
				h := 14 + int(b[14]&15)*4
				if h+244 > len(b) || binary.BigEndian.Uint16(b[h+2:h+4]) != 67 || b[h+8] != 1 {
					continue
				}
				if b[h+11] > 4 {
					captures[0].mu.Unlock()
					t.Fatal("packet capture shows a request beyond the hop limit")
				}
				forwarded = forwarded || net.IP(b[h+32:h+36]).String() == iface.Address && b[h+11] == 1
			}
			captures[0].mu.Unlock()
			if !forwarded {
				return false
			}
			captures[i+1].mu.Lock()
			found := false
			for _, packet := range captures[i+1].packets {
				b := packet.data
				if len(b) < 278 || b[12] != 8 || b[13] != 0 || b[23] != 17 {
					continue
				}
				h := 14 + int(b[14]&15)*4
				if h+244 > len(b) || binary.BigEndian.Uint16(b[h+2:h+4]) != 68 || b[h+8] != 2 {
					continue
				}
				broadcast := b[30] == 255 && b[31] == 255 && b[32] == 255 && b[33] == 255
				found = found || broadcast == (i == 1)
			}
			captures[i+1].mu.Unlock()
			if !found {
				return false
			}
		}
		return true
	})
	t.Log("independent clients acquired both remote scopes; large unicast/broadcast DHCPINFORM reassembled across MTU 600 with intact options, packet fragment checks, giaddr/hops, hop-limit discard, TCP/UDP and worker restart passed")
}

func startFixtureDNSMasq(t *testing.T, namespace netns.NsHandle, args []string) *exec.Cmd {
	t.Helper()
	dnsmasq, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Fatal("independent dnsmasq server required", err)
	}
	work := t.TempDir()
	log, err := os.Create(filepath.Join(work, "dnsmasq.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	// Debug mode avoids setgroups() in this isolated user namespace.
	base := []string{"--no-daemon", "--conf-file=/dev/null", "--port=0", "--interface=server0", "--bind-interfaces", "--dhcp-authoritative", "--log-dhcp", "--log-facility=-", "--pid-file=", "--dhcp-leasefile=" + filepath.Join(work, "leases")}
	command := exec.CommandContext(ctx, dnsmasq, append(base, args...)...)
	command.Stdout, command.Stderr = log, log
	if err := fixtureInsideNamespace(namespace, command.Start); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { cancel(); <-done })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("independent server: %s\n%s", args, data)
		}
	})
	return command
}

type fixtureDHCPRelayLease struct{ Address, Server, Router, Domain string }

func acquireFixtureDHCPRelayLease(t *testing.T, namespace netns.NsHandle, iface string, broadcast bool, requested string) fixtureDHCPRelayLease {
	t.Helper()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("independent BusyBox udhcpc required", err)
	}
	work := t.TempDir()
	report, script := filepath.Join(work, "report"), filepath.Join(work, "lease.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in bound|renew) printf '%s\\n' \"$ip\" \"$serverid\" \"$router\" \"$domain\" > \"$ROUTERD_RELAY_REPORT\";; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"udhcpc", "-f", "-n", "-q", "-t", "5", "-T", "1", "-i", iface, "-s", script, "-p", filepath.Join(work, "pid")}
	if broadcast {
		args = append(args, "-B")
	}
	if requested != "" {
		args = append(args, "-r", requested)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, busybox, args...)
	command.Env = append(os.Environ(), "ROUTERD_RELAY_REPORT="+report)
	var output strings.Builder
	command.Stdout, command.Stderr = &output, &output
	if err := fixtureInsideNamespace(namespace, command.Start); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("independent client %s: %v\n%s", iface, err, output.String())
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(fields) != 4 {
		t.Fatal("independent client did not report all DHCP fields", fields)
	}
	return fixtureDHCPRelayLease{Address: fields[0], Server: fields[1], Router: fields[2], Domain: fields[3]}
}
