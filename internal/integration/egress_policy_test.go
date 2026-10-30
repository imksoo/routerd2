//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
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
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

//go:embed testdata/egress-policy.yaml
var legacyEgressPolicy []byte

//go:embed testdata/egress-policy-options.yaml
var legacyEgressOptions []byte

// Exercise the homert 120/80/60 grouping and source-address rendezvous through
// migrated operator intent, product workers and five independent kernel peers.
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
		captures = append(captures, healthPBRCapture(t, fmt.Sprintf("wan%d", i)))
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
	if err := h.RouteAdd(&netlink.Route{LinkIndex: client.Attrs().Index, Gw: net.ParseIP("198.18.0.254")}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	input, options := filepath.Join(work, "input.yaml"), filepath.Join(work, "options.yaml")
	if err := os.WriteFile(input, legacyEgressPolicy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options, legacyEgressOptions, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", options, input)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	body, err := command.Output()
	if err != nil {
		t.Fatal(err, diagnostics.String())
	}
	program, err := config.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	program.Resources = append(program.Resources,
		resourceprogram.Resource{ID: "prior-route-interface", Interface: &network.Interface{Name: "tailscale0"}},
		resourceprogram.Resource{ID: "prior-route-address", Address: &resourceprogram.Address{Interface: "tailscale0", CIDR: "100.64.0.2/24"}},
		resourceprogram.Resource{ID: "prior-route", Route: &network.Route{Destination: "100.64.0.1/32", Interface: "tailscale0", Table: 52}},
		resourceprogram.Resource{ID: "prior-rule", PolicyRule: &network.PolicyRule{Family: 4, Priority: 5270, Table: 52}},
		resourceprogram.Resource{ID: "main-fallthrough", Route: &network.Route{Destination: "198.51.100.20/32", Interface: "wan4", Gateway: "198.19.3.1"}},
	)
	for i := range program.Resources {
		if original := program.Resources[i].Firewall; original != nil {
			f := *original
			chain := *f.ForwardChain
			chain.Rules = append(append([]network.FirewallRule(nil), chain.Rules...),
				network.FirewallRule{Match: network.PacketMatch{InInterface: "lan0", OutInterface: "tailscale0"}, Action: "accept"},
				network.FirewallRule{Match: network.PacketMatch{InInterface: "lan-ha", OutInterface: "tailscale0"}, Action: "accept"},
			)
			f.ForwardChain = &chain
			program.Resources[i].Firewall = &f
		}
	}
	// A real election with no reachable WAN must wait for an applied egress.
	for _, probe := range probes {
		probe.Close()
	}
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
	until(t, 6*time.Second, func() bool {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+election, nil, &status); err != nil {
			t.Fatal(err)
		}
		var snapshot vrrp.Snapshot
		_ = json.Unmarshal(status.Snapshot, &snapshot)
		if snapshot.Active {
			t.Fatal("gateway published without a ready WAN")
		}
		return snapshot.State == vrrp.StateActive && snapshot.ActivationState == "Preparing"
	})
	for i, peer := range peers {
		probes[i] = healthPBRListener(t, peer, "198.51.100.10:18080", "probe")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		_ = fixtureInsideNamespace(lan, func() error {
			for _, args := range [][]string{{"-4", "address", "show"}, {"-4", "route", "show"}, {"-4", "neigh", "show"}} {
				output, err := exec.Command("ip", args...).CombinedOutput()
				t.Logf("client %v: %s %v", args, output, err)
			}
			return nil
		})
		output, err := exec.Command("ip", "-4", "address", "show", "dev", "lan-ha").CombinedOutput()
		t.Logf("gateway: %s %v", output, err)
		var page network.DesiredPage
		err = f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &page)
		t.Logf("writer: %s %v", page.Data, err)
		var status runtimeplan.Status
		err = f.control.Do(ctx, "GET", "/v1/services", nil, &status)
		body, _ := json.Marshal(status)
		t.Logf("runtime: %s %v", body, err)
		for _, resource := range program.Resources {
			if resource.Service != nil && resource.Service.Health != nil {
				var instance runtimeplan.InstanceStatus
				err := f.control.Do(ctx, "GET", "/v1/services/"+resource.Service.ModuleName(), nil, &instance)
				body, _ := json.Marshal(instance)
				t.Logf("probe: %s %v", body, err)
			}
		}
	})
	waitMarks := func(want ...uint32) {
		t.Helper()
		until(t, 8*time.Second, func() bool {
			var page network.DesiredPage
			if err := f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &page); err != nil {
				t.Fatal(err)
			}
			var desired network.Desired
			if err := json.Unmarshal(page.Data, &desired); err != nil {
				t.Fatal(err)
			}
			if desired.Firewall == nil || len(desired.Firewall.FlowRoutes) != 1 {
				return false
			}
			marks := map[uint32]bool{}
			for _, mark := range desired.Firewall.FlowRoutes[0].Buckets {
				marks[mark] = true
			}
			if len(marks) != len(want) {
				return false
			}
			for _, mark := range want {
				if !marks[mark] {
					return false
				}
			}
			return true
		})
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
	waitGateway := func() {
		t.Helper()
		until(t, 5*time.Second, func() bool {
			var status runtimeplan.InstanceStatus
			if err := f.control.Do(ctx, "GET", "/v1/services/"+election, nil, &status); err != nil {
				t.Fatal(err)
			}
			var published vrrp.Snapshot
			if err := json.Unmarshal(status.Snapshot, &published); err != nil {
				t.Fatal(err)
			}
			if !published.Active {
				return false
			}
			addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
			if err != nil {
				t.Fatal(err)
			}
			for _, address := range addresses {
				if address.IP.Equal(net.ParseIP("198.18.0.254")) {
					return true
				}
			}
			return false
		})
	}
	waitMarks(272, 273, 274)
	waitGateway()
	blocked, err := net.Listen("tcp4", "192.0.2.2:18096")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blocked.Close() })
	if err := fixtureInsideNamespace(peers[0], func() error {
		connection, err := net.DialTimeout("tcp4", "192.0.2.2:18096", 300*time.Millisecond)
		if err == nil {
			connection.Close()
			return fmt.Errorf("untrust peer reached a real WAN listener")
		}
		if failure, ok := err.(net.Error); !ok || !failure.Timeout() {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if captures[0].count("192.0.2.1", "192.0.2.2", 6, 18096) == 0 {
		t.Fatal("WAN denial probe did not reach the product interface")
	}
	existing := traffic("ABC")
	for i := 0; i < 3; i++ {
		probes[i].Close()
	}
	waitMarks(275)
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
	waitMarks(276)
	traffic("E")
	// The direct final fallback models the homert IX path. Small path replies
	// do not establish that TCP can carry data through its migrated ACL/NAT.
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
	waitMarks()
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
	waitMarks(276)
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
	for time.Since(transition) < 57*time.Second {
		waitMarks(276)
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
			t.Fatal("recovered too early", got, time.Since(transition))
		}
		conn.Close()
		time.Sleep(time.Second)
	}
	waitMarks(272, 273, 274)
	if time.Since(transition) < 59*time.Second {
		t.Fatal("60-second recovery hold was not honored", time.Since(transition))
	}
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
	waitMarks(275)
	traffic("D")
	until(t, 10*time.Second, func() bool {
		state = control.State{}
		return f.control.Do(ctx, "GET", "/v1/config", nil, &state) == nil && state.Revision == 3 && state.Confirmation == nil
	})
	waitMarks(272, 273, 274)
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
	waitMarks() // Health expiry blocks new flows before the election expires.
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
	// The main route remains usable, but every expired candidate must stop a
	// restored connection mark within its own table rather than fall through.
	if routes, err := netlink.RouteGet(net.ParseIP("198.51.100.20")); err != nil || len(routes) != 1 {
		t.Fatalf("main fallback setup: %+v %v", routes, err)
	}
	for mark := 272; mark <= 276; mark++ {
		if routes, err := netlink.RouteGetWithOptions(net.ParseIP("198.51.100.20"), &netlink.RouteGetOptions{Mark: uint32(mark)}); err == nil {
			t.Fatalf("expired mark %d escaped to main: %+v", mark, routes)
		}
	}
	t.Log("expired WAN marks stopped at their blackholes while an unmarked main route remained")
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
	waitMarks()
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
	waitMarks(272, 273, 274)
	waitGateway()
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
