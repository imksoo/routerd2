//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func connectionMarkedEgressProgram() config.Document {
	var interfaces []network.Interface
	var services []serviceconfig.Service
	var forwards []network.Forward
	var nat []network.NATRule
	var probeRoutes []network.Route
	var probeRules []network.PolicyRule
	var balanced []resourceprogram.EgressTarget
	var groups []resourceprogram.EgressGroup
	for i, block := range []string{"192.0.2", "203.0.113", "198.19.1", "198.19.2", "198.19.3"} {
		name := fmt.Sprintf("wan%d", i)
		interfaces = append(interfaces, network.Interface{Name: name, AdminState: network.AdminUp,
			Addresses: []string{block + ".2/24"}})
		services = append(services, serviceconfig.Service{Name: fmt.Sprintf("health-%d", i),
			Health: &health.Config{Resource: fmt.Sprintf("health-%d", i), Interface: name, SourceAddress: block + ".2", Mark: uint32(512 + i), Target: "198.51.100.10",
				Protocol: "tcp", Port: 18080, IntervalMillis: 250, TimeoutMillis: 250, HealthyThreshold: 1, UnhealthyThreshold: 2}})
		probeMark, probeTable := uint32(512+i), 120+i
		probeRules = append(probeRules, network.PolicyRule{Family: 4, Priority: 10000 + i, Table: probeTable, Mark: &probeMark})
		probeRoutes = append(probeRoutes, network.Route{Destination: "198.51.100.10/32", Gateway: block + ".1", Interface: name, Table: probeTable})
		forwards = append(forwards, network.Forward{From: "lan0", To: name})
		match := network.PacketMatch{Family: 4, Source: "198.18.0.0/24", OutInterface: name}
		rule := network.NATRule{Type: "masquerade", Match: match}
		if i == 0 {
			rule.Type, rule.Address = "snat", block+".2"
		}
		nat = append(nat, rule)
		target := resourceprogram.EgressTarget{Name: fmt.Sprintf("path%d", i), Interface: name, Gateway: block + ".1",
			Table: 110 + i, Priority: 10110 + i, Mark: uint32(272 + i), When: &resourceprogram.FactCondition{Fact: fmt.Sprintf("service.health-%d.active", i)}}
		if i < 3 {
			balanced = append(balanced, target)
		} else {
			name, weight := "secondary", 80
			if i == 4 {
				name, weight = "last", 60
			}
			groups = append(groups, resourceprogram.EgressGroup{Name: name, Weight: weight, Targets: []resourceprogram.EgressTarget{target}})
		}
	}
	groups = append([]resourceprogram.EgressGroup{{Name: "balanced", Weight: 120, Targets: balanced}}, groups...)
	interfaces = append(interfaces, network.Interface{Name: "lan0", AdminState: network.AdminUp, Addresses: []string{"198.18.0.1/24"}})
	forwards = append(forwards, network.Forward{From: "lan0", To: "tailscale0"})
	forwarding := true
	services = append(services, serviceconfig.Service{Name: "gateway", VRRP: &vrrp.Config{Interface: "lan0", VIPInterface: "lan-ha", Family: "ipv4",
		SourceAddress: "198.18.0.1", VirtualRouterID: 43, Priority: 100, AdvertIntervalCentiseconds: 100,
		AcceptMode: true, Addresses: []string{"198.18.0.254/24"}, GracefulActivation: &vrrp.GracefulActivation{ReadyWhen: resourceprogram.FactCondition{Fact: "service.health-0.active"}, Timeout: "5s"}}})
	firewall := &network.Firewall{Forward: forwards, NAT: nat}
	// Each independent provider owns the same probe VIP. Probe marks keep the
	// real sockets on their selected WAN; the main route gives loose rp_filter
	// a return path for replies arriving on any provider interface.
	probeRoutes = append(probeRoutes, network.Route{Destination: "198.51.100.10/32", Gateway: "192.0.2.1", Interface: "wan0"})
	program := config.FromNative(config.Native{Network: network.Desired{Interfaces: interfaces, Routes: probeRoutes, PolicyRules: probeRules,
		Forwarding: network.Forwarding{IPv4: &forwarding}, Firewall: firewall}, Services: services})
	program.Resources = append(program.Resources, resourceprogram.Resource{ID: "default-egress", When: &resourceprogram.FactCondition{Fact: "service.gateway.elected"},
		EgressPolicy: &resourceprogram.EgressPolicy{Sources: []string{"198.18.0.0/24"}, Destinations: []string{"0.0.0.0/0"},
			ExcludeDestinations: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "198.18.0.0/24"},
			HysteresisMillis:    60000, Groups: groups}})
	return program
}

// Exercise the homert 120/80/60 grouping and source-address rendezvous through
// native operator intent, product workers and five independent kernel peers.
func TestDaemonConnectionMarkedEgressPolicy(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonConnectionMarkedEgressPolicy", 150*time.Second) {
		return
	}
	blocks := []string{"192.0.2", "203.0.113", "198.19.1", "198.19.2", "198.19.3"}
	var peers []netns.NsHandle
	var probes []net.Listener
	var captures []*healthPBRCaptureState
	for i, block := range blocks {
		peer := healthPBRPeer(t, fmt.Sprintf("wan%d", i), fmt.Sprintf("provider%d", i), block+".1/24", block+".2")
		peers = append(peers, peer)
		probes = append(probes, healthPBRListener(t, peer, "198.51.100.10:18080", "probe"))
		vrrpVIPResponders(t, peer, "4", []string{"198.51.100.20"}, string(rune('A'+i)))
		egressTCPPeer(t, peer, byte('A'+i))
		captures = append(captures, healthPBRPortsCapture(t, fmt.Sprintf("wan%d", i), []uint16{18080, 18094, 18095}))
	}
	// An independent peer behind an earlier RPDB lookup models a route owned
	// by another routing engine. PBR must not reject its selected interface.
	tail := healthPBRPeer(t, "tailscale0", "tailpeer", "100.64.0.1/24", "100.64.0.2")
	vrrpVIPResponders(t, tail, "4", []string{"100.64.0.1"}, "T")
	healthPBRListener(t, tail, "100.64.0.1:18095", "T")
	lan := fixturePeerNamespace(t, "lan0", "client0")
	h, err := netlink.NewHandleAt(lan, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	client, err := h.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	for host := 2; host < 66; host++ {
		address, _ := netlink.ParseAddr(fmt.Sprintf("198.18.0.%d/24", host))
		if err := h.AddrAdd(client, address); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: client.Attrs().Index, Gw: net.ParseIP("198.18.0.1")}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	program := connectionMarkedEgressProgram()
	program.Resources = append(program.Resources,
		resourceprogram.Resource{ID: "prior-route-interface", Interface: &network.Interface{Name: "tailscale0"}},
		resourceprogram.Resource{ID: "prior-route-address", Address: &resourceprogram.Address{Interface: "tailscale0", CIDR: "100.64.0.2/24"}},
		resourceprogram.Resource{ID: "prior-route", Route: &network.Route{Destination: "100.64.0.1/32", Interface: "tailscale0", Table: 52}},
		resourceprogram.Resource{ID: "prior-rule", PolicyRule: &network.PolicyRule{Family: 4, Priority: 5270, Table: 52}},
		resourceprogram.Resource{ID: "main-fallthrough", Route: &network.Route{Destination: "198.51.100.20/32", Interface: "wan4", Gateway: "198.19.3.1"}},
	)
	f := startFixtureDaemon(t, program)
	until(t, 3*time.Second, func() bool {
		link, err := netlink.LinkByName("lan0")
		if err != nil {
			t.Fatal(err)
		}
		return link.Attrs().Flags&net.FlagUp != 0
	})
	captureDir := os.Getenv("ROUTERD2_TEST_EVIDENCE_DIR")
	if captureDir == "" {
		captureDir = work
	}
	// The LAN aggregates all 64 clients and five exits. Stream its headers to
	// disk instead of retaining every frame in the per-WAN in-memory fixture.
	lanCapture := exec.Command("tcpdump", "-U", "-n", "-s", "128", "-i", "lan0", "-w", filepath.Join(captureDir, "lan0.pcap"))
	var captureLog bytes.Buffer
	lanCapture.Stderr = &captureLog
	if err := lanCapture.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lanCapture.Process.Signal(os.Interrupt); err != nil {
			t.Error(err)
		}
		if err := lanCapture.Wait(); err != nil {
			t.Error(err)
		}
		t.Logf("LAN capture: %s", captureLog.String())
	})
	ctx := context.Background()
	var election string
	for _, resource := range program.Resources {
		if resource.Service != nil && resource.Service.VRRP != nil {
			election = resource.Service.ModuleName()
		}
	}
	dial := func(protocol string, host int) net.Conn {
		t.Helper()
		source := net.IPv4(198, 18, 0, byte(host))
		dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: source}}
		port := "18095"
		if protocol == "udp4" {
			dialer.LocalAddr = &net.UDPAddr{IP: source}
			port = "18094"
		}
		var conn net.Conn
		if err := fixtureInsideNamespace(lan, func() error { var err error; conn, err = dialer.Dial(protocol, "198.51.100.20:"+port); return err }); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	read := func(conn net.Conn, send bool) string {
		t.Helper()
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if send {
			if _, err := conn.Write([]byte("path")); err != nil {
				t.Fatal(err)
			}
		}
		var reply [1]byte
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			t.Fatal(err)
		}
		return string(reply[:])
	}
	traffic := func(allowed string) map[string]egressConnections {
		t.Helper()
		seen := map[string]egressConnections{}
		for host := 2; host < 66; host++ {
			tcp := dial("tcp4", host)
			path := read(tcp, true)
			udp := dial("udp4", host)
			udpPath := read(udp, true)
			if path != udpPath || !bytes.ContainsRune([]byte(allowed), rune(path[0])) {
				t.Fatalf("source %d TCP=%s UDP=%s, want %s", host, path, udpPath, allowed)
			}
			if _, exists := seen[path]; exists {
				udp.Close()
				tcp.Close()
			} else {
				seen[path] = egressConnections{tcp: tcp, udp: udp}
				t.Cleanup(func() { udp.Close(); tcp.Close() })
			}
		}
		if len(seen) != len(allowed) {
			t.Fatal("missing actual exit", allowed, seen)
		}
		return seen
	}
	probePath := func(protocol string) (string, error) {
		source := net.IPv4(198, 18, 0, 2)
		port := "18095"
		var local net.Addr = &net.TCPAddr{IP: source}
		if protocol == "udp4" {
			local = &net.UDPAddr{IP: source}
			port = "18094"
		}
		var conn net.Conn
		err := fixtureInsideNamespace(lan, func() error {
			var err error
			conn, err = (&net.Dialer{Timeout: 300 * time.Millisecond, LocalAddr: local}).Dial(protocol, "198.51.100.20:"+port)
			return err
		})
		if err != nil {
			return "", err
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return "", err
		}
		if _, err := conn.Write([]byte("path")); err != nil {
			return "", err
		}
		var reply [1]byte
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return "", err
		}
		return string(reply[:]), nil
	}
	waitPath := func(allowed string) {
		t.Helper()
		until(t, 8*time.Second, func() bool {
			tcp, err := probePath("tcp4")
			if err != nil {
				return false
			}
			udp, udpErr := probePath("udp4")
			return udpErr == nil && tcp == udp && bytes.ContainsRune([]byte(allowed), rune(tcp[0]))
		})
	}
	waitNoPath := func() {
		t.Helper()
		until(t, 8*time.Second, func() bool {
			_, tcpErr := probePath("tcp4")
			_, udpErr := probePath("udp4")
			return tcpErr != nil && udpErr != nil
		})
	}
	waitGateway := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		var last runtimeplan.InstanceStatus
		var controlErr error
		for time.Now().Before(deadline) {
			var status runtimeplan.InstanceStatus
			if err := f.control.Do(ctx, "GET", "/v1/services/"+election, nil, &status); err != nil {
				controlErr = err
				time.Sleep(25 * time.Millisecond)
				continue
			}
			last = status
			if len(status.Snapshot) == 0 {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			var published vrrp.Snapshot
			if err := json.Unmarshal(status.Snapshot, &published); err != nil {
				t.Fatal(err)
			}
			if published.Active {
				addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
				if err != nil {
					t.Fatal(err)
				}
				for _, address := range addresses {
					if address.IP.Equal(net.ParseIP("198.18.0.254")) {
						return
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		var services runtimeplan.Status
		servicesErr := f.control.Do(ctx, "GET", "/v1/services", nil, &services)
		body, _ := json.Marshal(services)
		t.Fatalf("gateway did not regain the VIP: last=%s control_error=%v services=%s services_error=%v", last.Snapshot, controlErr, body, servicesErr)
	}
	waitGateway()
	waitPath("ABC")
	existing := traffic("ABC")
	for i := 0; i < 3; i++ {
		probes[i].Close()
	}
	waitPath("D")
	traffic("D")
	for path, conn := range existing {
		if got := read(conn.udp, true); got != path {
			t.Fatal("existing UDP connection moved", path, got)
		}
		if got := read(conn.tcp, true); got != path {
			t.Fatal("existing TCP connection moved", path, got)
		}
	}
	probes[3].Close()
	waitPath("E")
	traffic("E")
	// The direct final fallback models the homert IX path and carries a full TCP payload.
	bulk := dial("tcp4", 2)
	if err := bulk.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := bulk.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var mss int
	var socketErr error
	if err := raw.Control(func(fd uintptr) { mss, socketErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG) }); err != nil || socketErr != nil {
		t.Fatal("TCP MSS", err, socketErr)
	}
	exitLink, err := netlink.LinkByName("wan4")
	if err != nil || mss <= 0 || mss > exitLink.Attrs().MTU-40 {
		t.Fatal("direct exit MSS", mss, err)
	}
	if _, err := bulk.Write([]byte("bulk")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(bulk, bytes.NewReader(bytes.Repeat([]byte{'u'}, 1<<20))); err != nil {
		t.Fatal("direct exit upload", err)
	}
	reply := make([]byte, 1<<20)
	if _, err := io.ReadFull(bulk, reply); err != nil || !bytes.Equal(reply, bytes.Repeat([]byte{'E'}, len(reply))) {
		t.Fatal("direct exit download", err)
	}
	t.Logf("direct final fallback: 1 MiB upload/download with exact payload, TCP_MAXSEG=%d, link MTU=%d, endpoints=%s -> %s", mss, exitLink.Attrs().MTU, bulk.LocalAddr(), bulk.RemoteAddr())
	bulk.Close()
	priorTraffic := func() {
		for _, protocol := range []string{"tcp4", "udp4"} {
			port := "18095"
			var source net.Addr = &net.TCPAddr{IP: net.ParseIP("198.18.0.2")}
			if protocol == "udp4" {
				port = "18094"
				source = &net.UDPAddr{IP: net.ParseIP("198.18.0.2")}
			}
			var conn net.Conn
			if err := fixtureInsideNamespace(lan, func() error {
				var err error
				conn, err = (&net.Dialer{Timeout: time.Second, LocalAddr: source}).Dial(protocol, "100.64.0.1:"+port)
				return err
			}); err != nil {
				t.Fatal("earlier RPDB route", protocol, err)
			}
			if got := read(conn, protocol == "udp4"); got != "T" {
				t.Fatal("earlier RPDB route", protocol, got)
			}
			conn.Close()
		}
	}
	priorTraffic()
	probes[4].Close()
	waitNoPath()
	priorTraffic()
	blockedUDP := dial("udp4", 65)
	defer blockedUDP.Close()
	payload := []byte("routerd-no-path-main-probe")
	if err := blockedUDP.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := blockedUDP.Write(payload); err != nil {
		t.Fatal(err)
	}
	var blockedReply [64]byte
	if _, err := blockedUDP.Read(blockedReply[:]); err == nil {
		t.Fatal("no-WAN UDP escaped through main")
	} else if failure, ok := err.(net.Error); !ok || !failure.Timeout() {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(lan, func() error {
		conn, err := (&net.Dialer{Timeout: 300 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: net.ParseIP("198.18.0.65")}}).Dial("tcp4", "198.51.100.20:18095")
		if err == nil {
			conn.Close()
			return fmt.Errorf("no-WAN TCP escaped through main")
		}
		if failure, ok := err.(net.Error); !ok || !failure.Timeout() {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, capture := range captures {
		capture.mu.Lock()
		for _, packet := range capture.packets {
			if bytes.Contains(packet.data, payload) {
				capture.mu.Unlock()
				t.Fatalf("no-path probe leaked onto WAN%d", i)
			}
		}
		capture.mu.Unlock()
	}
	probes[4] = healthPBRListener(t, peers[4], "198.51.100.10:18080", "restored")
	waitPath("E")
	if got := read(blockedUDP, true); got != "E" {
		t.Fatal("no-path packet mark prevented recovery of the same UDP socket", got)
	}
	transition := time.Now()
	t.Log("earlier table52 TCP/UDP survived all WAN failures; main leaked no UDP probe; same socket recovered")
	for i := 0; i < 4; i++ {
		probes[i] = healthPBRListener(t, peers[i], "198.51.100.10:18080", "restored")
	}
	// routerd's hysteresis holds a still-ready selection for 60 seconds from
	// its last transition; failure never waits for this recovery hold.
	for time.Since(transition) < 60*time.Second {
		for path, conn := range existing {
			if got := read(conn.tcp, true); got != path {
				t.Fatal("existing TCP moved during hold", path, got)
			}
			if got := read(conn.udp, true); got != path {
				t.Fatal("existing UDP moved during hold", path, got)
			}
		}
		conn := dial("udp4", 2)
		if got := read(conn, true); got != "E" {
			var page network.DesiredPage
			var desired network.Desired
			_ = f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &page)
			_ = json.Unmarshal(page.Data, &desired)
			marks := map[uint32]int{}
			if desired.Firewall != nil {
				for _, flow := range desired.Firewall.FlowRoutes {
					for _, mark := range flow.Buckets {
						marks[mark]++
					}
				}
			}
			t.Fatal("recovered too early", got, time.Since(transition), "selected flow marks", marks)
		}
		conn.Close()
		time.Sleep(time.Second)
	}
	waitPath("ABC")
	traffic("ABC")
	for path, conn := range existing {
		if got := read(conn.udp, true); got != path {
			t.Fatal("retained UDP connection changed after recovery", path, got)
		}
		if got := read(conn.tcp, true); got != path {
			t.Fatal("retained TCP connection changed after recovery", path, got)
		}
	}
	trial := program
	trial.Resources = append([]resourceprogram.Resource(nil), program.Resources...)
	for i, resource := range trial.Resources {
		if resource.EgressPolicy == nil {
			continue
		}
		changed := *resource.EgressPolicy
		changed.Groups = append([]resourceprogram.EgressGroup(nil), changed.Groups...)
		changed.Groups[1].Weight = 160
		changed.HysteresisMillis = 0
		trial.Resources[i].EgressPolicy = &changed
	}
	var state control.State
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "egress-marked-trial", Config: trial, ConfirmTimeoutSeconds: 5}, &state); err != nil {
		t.Fatal(err)
	}
	waitPath("D")
	traffic("D")
	until(t, 10*time.Second, func() bool {
		state = control.State{}
		return f.control.Do(ctx, "GET", "/v1/config", nil, &state) == nil && state.Revision == 3 && state.Confirmation == nil
	})
	waitPath("ABC")
	traffic("ABC")
	var owner runtimeplan.InstanceStatus
	if err := f.control.Do(ctx, "GET", "/v1/services/"+election, nil, &owner); err != nil {
		t.Fatal(err)
	}
	var grant vrrp.Snapshot
	if err := json.Unmarshal(owner.Snapshot, &grant); err != nil {
		t.Fatal(err)
	}
	var processes supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
		t.Fatal(err)
	}
	var writer supervisor.ModuleStatus
	var frozen []supervisor.ModuleStatus
	for _, process := range processes.Modules {
		if process.Name == "network" {
			writer = process
		}
		if process.Name == "control" || process.Role == "health" {
			if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			frozen = append(frozen, process)
			t.Cleanup(func() { _ = syscall.Kill(process.PID, syscall.SIGCONT) })
		}
	}
	var applied network.DesiredPage
	if err := f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &applied); err != nil {
		t.Fatal(err)
	}
	var desired network.Desired
	if err := json.Unmarshal(applied.Data, &desired); err != nil {
		t.Fatal(err)
	}
	var authority lifetime.Deadline
	for _, link := range desired.Links {
		if link.Name == "lan-ha" {
			authority = link.Authority
		}
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	if authority.BootID != clock.BootID || authority.Nanos <= uint64(clock.BoottimeNanoseconds) {
		t.Fatal("missing live applied gateway authority", authority, clock)
	}
	t.Logf("frozen election observed=%d expires=%d; applied gateway expires=%d; now=%d", grant.ObservedNanos, grant.ExpiresNanos, authority.Nanos, clock.BoottimeNanoseconds)
	for path, conn := range existing {
		if got := read(conn.udp, true); got != path {
			t.Fatal("live gateway authority lost established UDP", path, got)
		}
	}
	waitNoPath() // Health expiry blocks new flows before the election expires.
	until(t, 5*time.Second, func() bool {
		clock, err = boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if authority.Live(clock) {
			return false
		}
		link, err := netlink.LinkByName("lan-ha")
		if err != nil {
			t.Fatal(err)
		}
		return link.Attrs().Flags&net.FlagUp == 0
	})
	t.Log("expired health and gateway authority stopped new LAN flows before the applied VIP expired")
	t.Logf("gateway fenced at boottime=%d, original applied expiry=%d", clock.BoottimeNanoseconds, authority.Nanos)
	if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		var current supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &current); err != nil {
			t.Fatal(err)
		}
		for _, process := range current.Modules {
			if process.Name == "network" {
				return process.Ready && process.Generation > writer.Generation
			}
		}
		return false
	})
	// Successful new TCP would demonstrate that restart renewed stale authority.
	var unexpected net.Conn
	err = fixtureInsideNamespace(lan, func() error {
		var err error
		unexpected, err = net.DialTimeout("tcp4", "198.51.100.20:18095", 300*time.Millisecond)
		return err
	})
	if err == nil {
		unexpected.Close()
		t.Fatal("expired flow authority allowed a new TCP connection after writer restart")
	}
	// This graph explicitly owns the gateway and egress through the election.
	// Its expired owner must stop forwarding even established marked flows.
	for _, conn := range existing {
		if err := conn.udp.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.udp.Write([]byte("path")); err != nil {
			t.Fatal(err)
		}
		var reply [1]byte
		if _, err := conn.udp.Read(reply[:]); err == nil {
			t.Fatal("expired election still forwarded an established UDP flow")
		} else if failure, ok := err.(net.Error); !ok || !failure.Timeout() {
			t.Fatal(err)
		}
	}
	for _, process := range frozen {
		if err := syscall.Kill(process.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	waitGateway()
	waitPath("ABC")
	traffic("ABC")
	for path, conn := range existing {
		if got := read(conn.tcp, true); got != path {
			t.Fatal("recovered owner or writer restart moved existing TCP", path, got)
		}
		if got := read(conn.udp, true); got != path {
			t.Fatal("recovered owner or writer restart moved existing UDP", path, got)
		}
	}
	for i, capture := range captures {
		for _, packet := range []struct {
			target   string
			protocol byte
			port     uint16
		}{{"198.51.100.10", 6, 18080}, {"198.51.100.20", 6, 18095}, {"198.51.100.20", 17, 18094}} {
			if capture.count(blocks[i]+".2", packet.target, packet.protocol, packet.port) == 0 {
				t.Fatalf("WAN%d missing actual packet %+v", i, packet)
			}
		}
	}
	t.Logf("three exits, 120→80→60, immediate failure, existing TCP/UDP and recovery hold %s passed", time.Since(transition))
}

type egressConnections struct{ tcp, udp net.Conn }

func egressTCPPeer(t *testing.T, ns netns.NsHandle, reply byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var listener net.Listener
	if err := fixtureInsideNamespace(ns, func() error { var err error; listener, err = net.Listen("tcp4", "198.51.100.20:18095"); return err }); err != nil {
		cancel()
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				var b [4]byte
				for {
					if _, err := io.ReadFull(conn, b[:]); err != nil {
						return
					}
					if string(b[:]) == "bulk" {
						body := make([]byte, 1<<20)
						if _, err := io.ReadFull(conn, body); err != nil {
							return
						}
						if !bytes.Equal(body, bytes.Repeat([]byte{'u'}, len(body))) {
							t.Error("direct exit peer received different upload data")
							return
						}
						_, _ = io.Copy(conn, bytes.NewReader(bytes.Repeat([]byte{reply}, len(body))))
						return
					}
					if _, err := conn.Write([]byte{reply}); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); workers.Wait() })
}
