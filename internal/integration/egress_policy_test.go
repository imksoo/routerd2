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

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
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
	forward := true
	firewall := &network.Firewall{ForwardChain: &network.FirewallChain{Policy: "accept"}}
	for i := range blocks {
		firewall.Masquerade = append(firewall.Masquerade, network.Masquerade{Source: "198.18.0.0/24", OutInterface: fmt.Sprintf("wan%d", i)})
	}
	program.Resources = append(program.Resources, resourceprogram.Resource{ID: "forward", Forwarding: &network.Forwarding{IPv4: &forward}}, resourceprogram.Resource{ID: "firewall", Firewall: firewall})
	f := startFixtureDaemon(t, program)
	ctx := context.Background()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var page network.DesiredPage
		err := f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &page)
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
	waitMarks(272, 273, 274)
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
	transition := time.Now()
	traffic("E")
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
	waitMarks() // The writer independently blocks new flows at original expiry.
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
	for path, conn := range existing {
		if got := read(conn.tcp, true); got != path {
			t.Fatal("writer restart moved existing TCP", path, got)
		}
		if got := read(conn.udp, true); got != path {
			t.Fatal("writer restart moved existing UDP", path, got)
		}
	}
	for _, process := range frozen {
		if err := syscall.Kill(process.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	waitMarks(272, 273, 274)
	traffic("ABC")
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
				var b [16]byte
				for {
					if _, err := conn.Read(b[:]); err != nil {
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
