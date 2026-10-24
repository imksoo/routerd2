//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	mdns "github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// This gate covers the real packet -> DHCP journal/ACK -> coordinator -> DNS
// chain in separate processes. A stopped producer must lose DNS ownership even
// though its address lease has not expired, and recovery must reuse the journal
// without replacing the DNS listener or its existing TCP connection.
func TestDaemonDHCPDNSAndOwnershipExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonDHCPDNSAndOwnershipExpiry") {
		return
	}
	clientNS := fixturePeerNamespace(t, "lan0", "client0")
	dnsPort := port(t)
	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true, ProbeTimeoutMilliseconds: 50,
		Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", LeaseSeconds: 60},
	}
	dns := dnsconfig.RuntimeConfig{Resource: "dns",
		Spec:  api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: dnsPort}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"DNSZone/lan"}}}},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lab.example", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: "192.0.2.1"}}, DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/leases"}}}}},
	}
	doc := config.Document{SchemaVersion: 1,
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}, {Name: "dns", DNS: &dns}},
	}
	f := startFixtureDaemon(t, doc)
	work := t.TempDir()
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", int(clientNS)))
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	acquire := func() dhcp4.Snapshot {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestDaemonDHCPClientChild$", "-test.v", "-test.timeout=12s")
		cmd.Env = append(os.Environ(), "ROUTERD2_DAEMON_DHCP_CLIENT="+child, "ROUTERD2_DAEMON_DHCP_SERVER="+parent, "ROUTERD2_DAEMON_DHCP_WORK="+work)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := fixtureInsideNamespace(clientNS, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("native DHCP client: %v\n%s", err, output.String())
		}
		b, err := os.ReadFile(filepath.Join(work, "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		var lease dhcp4.Snapshot
		if err := json.Unmarshal(b, &lease); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	// Keep total queries below the DNS server's bounded per-TCP-connection
	// query budget while checking several seconds of ownership expiration.
	wait := func(timeout time.Duration, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("DHCP/DNS condition was not reached before deadline")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	lease := acquire()
	if (lease.CurrentAddress != "192.0.2.100" && lease.CurrentAddress != "192.0.2.101" && lease.CurrentAddress != "192.0.2.102") || lease.ServerID != "192.0.2.1" || lease.LeaseTimeSeconds != 60 {
		t.Fatalf("unexpected DHCP lease: %+v", lease)
	}
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", dnsPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dc := &mdns.Conn{Conn: conn}
	query := func(name string, typ uint16) []mdns.RR {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		q := new(mdns.Msg).SetQuestion(name, typ)
		if err := dc.WriteMsg(q); err != nil {
			t.Fatalf("DNS listener replaced or TCP write failed: %v", err)
		}
		r, err := dc.ReadMsg()
		if err != nil || r.Id != q.Id {
			t.Fatalf("DNS exchange failed: %v", err)
		}
		return r.Answer
	}
	hasAddress := func(name, address string) bool {
		for _, rr := range query(name, mdns.TypeA) {
			if a, ok := rr.(*mdns.A); ok && a.A.String() == address {
				return true
			}
		}
		return false
	}
	wait(5*time.Second, func() bool { return hasAddress("workstation.lab.example.", lease.CurrentAddress) })
	status := func() supervisor.Status {
		var s supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	pid := func(s supervisor.Status, name string) int {
		for _, m := range s.Modules {
			if m.Name == name {
				return m.PID
			}
		}
		return 0
	}
	before := status()
	serverPID, dnsPID := pid(before, "service.leases"), pid(before, "service.dns")
	if serverPID == 0 || dnsPID == 0 {
		t.Fatalf("native service processes missing: %+v", before)
	}
	if err := syscall.Kill(serverPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(serverPID, syscall.SIGKILL)
	wait(7*time.Second, func() bool { return !hasAddress("workstation.lab.example.", lease.CurrentAddress) })
	if !hasAddress("router.lab.example.", "192.0.2.1") || pid(status(), "service.dns") != dnsPID {
		t.Fatal("DHCP ownership loss interrupted static DNS or restarted the listener")
	}
	if err := syscall.Kill(serverPID, 0); err != nil {
		t.Fatalf("producer exited before independent ownership-expiry proof: %v", err)
	}
	if err := syscall.Kill(serverPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait(8*time.Second, func() bool {
		s := status()
		return pid(s, "service.leases") != 0 && pid(s, "service.leases") != serverPID && hasAddress("workstation.lab.example.", lease.CurrentAddress)
	})
	// The client uses INIT-REBOOT from its independent durable journal. Server
	// recovery must ACK the same address and preserve the original DNS process.
	recovered := acquire()
	if recovered.CurrentAddress != lease.CurrentAddress || pid(status(), "service.dns") != dnsPID {
		t.Fatal("DHCP/DNS restart did not preserve the lease and unaffected worker")
	}
	if !hasAddress("workstation.lab.example.", lease.CurrentAddress) {
		t.Fatal("renewed lease disappeared from the existing DNS TCP connection")
	}
}

func fixtureInsideNamespace(ns netns.NsHandle, fn func() error) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, err := netns.Get()
	if err != nil {
		return err
	}
	defer previous.Close()
	if err := netns.Set(ns); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, netns.Set(previous)) }()
	return fn()
}

func fixturePeerNamespace(t *testing.T, localName, peerName string) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	prior, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	ns, createErr := netns.New()
	restoreErr := netns.Set(prior)
	prior.Close()
	runtime.UnlockOSThread()
	if err := errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: localName}, PeerName: peerName}); err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(peer, int(ns)); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(ns, syscall.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, name := range []string{"lo", peerName} {
		link, err := h.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	return ns
}

func TestDaemonDHCPClientChild(t *testing.T) {
	expected := os.Getenv("ROUTERD2_DAEMON_DHCP_CLIENT")
	if expected == "" {
		t.Skip("only runs inside the daemon fixture's dedicated client namespace")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || current != expected || current == os.Getenv("ROUTERD2_DAEMON_DHCP_SERVER") || current == os.Getenv("ROUTERD2_PROTOCOL_PARENT") || os.Getenv("ROUTERD2_PROTOCOL_FIXTURE") != "TestDaemonDHCPDNSAndOwnershipExpiry" {
		t.Fatal("refusing DHCP client outside a verified dedicated namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var lease dhcp4.Snapshot
	err = dhcp4.Run(ctx, dhcp4.Config{Resource: "fixture", Interface: "client0", StatePath: filepath.Join(os.Getenv("ROUTERD2_DAEMON_DHCP_WORK"), "client.json"), Hostname: "workstation"}, dhcp4.EffectsFunc(func(_ context.Context, s dhcp4.Snapshot) error {
		if s.Active {
			lease = s
			cancel()
		}
		return nil
	}), nil)
	if !errors.Is(err, context.Canceled) || !lease.Active {
		t.Fatalf("native DHCP acquisition failed: %v", err)
	}
	if err := config.AtomicJSON(filepath.Join(os.Getenv("ROUTERD2_DAEMON_DHCP_WORK"), "report.json"), lease); err != nil {
		t.Fatal(err)
	}
}
