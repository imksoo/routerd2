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
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
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
	testDaemonDHCPDNSOwnership(t, false)
}

func TestDaemonMigratedDHCPDNSAndOwnershipExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonMigratedDHCPDNSAndOwnershipExpiry") {
		return
	}
	testDaemonDHCPDNSOwnership(t, true)
}

func testDaemonDHCPDNSOwnership(t *testing.T, migrated bool) {
	t.Helper()
	clientNS := fixturePeerNamespace(t, "lan0", "client0")
	dnsPort := port(t)
	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true, ProbeTimeoutMilliseconds: 50,
		Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", LeaseSeconds: 60},
	}
	dns := dnsconfig.RuntimeConfig{Resource: "dns",
		Spec:  api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: dnsPort}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"DNSZone/lan"}}}},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lab.example", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: "192.0.2.1"}}, DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/leases"}}}}},
	}
	native := config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}, {Name: "dns", DNS: &dns}},
	}
	doc := config.FromNative(native)
	if migrated {
		doc = migratedDHCPDNSFixtureDocument(t, dnsPort)
	}
	serverName, dnsName := "", ""
	plan, err := config.Compile(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range plan.Native.Services {
		if service.DHCPServer != nil {
			if serverName != "" {
				t.Fatal("fixture has more than one DHCP server")
			}
			serverName = "service." + service.Name
		}
		if service.DNS != nil {
			if dnsName != "" {
				t.Fatal("fixture has more than one DNS resolver")
			}
			dnsName = "service." + service.Name
		}
	}
	if serverName == "" || dnsName == "" {
		t.Fatal("fixture did not compile a DHCP server and DNS resolver")
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
	serverPID, dnsPID := pid(before, serverName), pid(before, dnsName)
	if serverPID == 0 || dnsPID == 0 {
		t.Fatalf("native service processes missing: %+v", before)
	}
	if err := syscall.Kill(serverPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(serverPID, syscall.SIGKILL)
	var accepted dhcpserverconfig.Snapshot
	wait(2*time.Second, func() bool {
		var source runtimeplan.InstanceStatus
		var projection dnsconfig.Input
		if err := f.control.Do(context.Background(), "GET", "/v1/services/"+serverName, nil, &source); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(source.Snapshot, &accepted); err != nil {
			t.Fatal(err)
		}
		if err := f.control.Do(context.Background(), "GET", "/v1/services/"+dnsName+"/dns-input", nil, &projection); err != nil {
			t.Fatal(err)
		}
		if len(projection.Sets) != 1 {
			return false
		}
		set := projection.Sets[0]
		return set.Epoch == accepted.Epoch && set.Sequence == accepted.Sequence && set.BootID == accepted.BootID && set.ObservedNanos == accepted.ObservedNanos && set.ExpiresNanos == accepted.ExpiresNanos
	})
	clock, err := boottime.Read()
	if err != nil || !dhcpserverconfig.CurrentObservation(accepted, clock) {
		t.Fatal("source observation already expired", err, accepted)
	}
	remaining := time.Duration(accepted.ExpiresNanos - uint64(clock.BoottimeNanoseconds))
	if remaining > 2*time.Second {
		time.Sleep(remaining - 2*time.Second)
	}
	controlPID := pid(before, "control")
	if controlPID == 0 {
		t.Fatal("missing control process")
	}
	// Stop both policy actors. Only DNS can remove the live answer, using the
	// exact deadline published before either actor was paused.
	for _, paused := range []int{controlPID, f.pid} {
		if err := syscall.Kill(paused, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer syscall.Kill(paused, syscall.SIGCONT)
	}
	wait(7*time.Second, func() bool { return !hasAddress("workstation.lab.example.", lease.CurrentAddress) })
	if !hasAddress("router.lab.example.", "192.0.2.1") || syscall.Kill(dnsPID, 0) != nil {
		t.Fatal("DHCP ownership loss interrupted static DNS or restarted the listener")
	}
	if err := syscall.Kill(serverPID, 0); err != nil {
		t.Fatalf("producer exited before independent ownership-expiry proof: %v", err)
	}
	for _, paused := range []int{controlPID, f.pid} {
		if err := syscall.Kill(paused, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	if pid(status(), dnsName) != dnsPID {
		t.Fatal("DNS restarted while its input actors were paused")
	}
	var expired runtimeplan.InstanceStatus
	if err := f.control.Do(context.Background(), "GET", "/v1/services/"+serverName, nil, &expired); err != nil {
		t.Fatal(err)
	}
	var original dhcpserverconfig.Snapshot
	if err := json.Unmarshal(expired.Snapshot, &original); err != nil || !expired.OwnershipExpired || !original.Active || len(original.Leases) != 1 || !time.Now().Before(original.Leases[0].ExpiresAt) {
		t.Fatal("current DNS eligibility and the publisher's last observation were not distinguished", err, expired, original)
	}
	if err := syscall.Kill(serverPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait(8*time.Second, func() bool {
		s := status()
		return pid(s, serverName) != 0 && pid(s, serverName) != serverPID && hasAddress("workstation.lab.example.", lease.CurrentAddress)
	})
	// The client uses INIT-REBOOT from its independent durable journal. Server
	// recovery must ACK the same address and preserve the original DNS process.
	recovered := acquire()
	if recovered.CurrentAddress != lease.CurrentAddress || pid(status(), dnsName) != dnsPID {
		t.Fatal("DHCP/DNS restart did not preserve the lease and unaffected worker")
	}
	if !hasAddress("workstation.lab.example.", lease.CurrentAddress) {
		t.Fatal("renewed lease disappeared from the existing DNS TCP connection")
	}
	t.Log("real DHCP ACK and persistent DNS TCP: original boot deadline removed DNS answers while server, control and supervisor were paused; journal recovery restored the lease on the same DNS connection")
	if migrated {
		t.Log("legacy resource YAML and explicit finite-source policy: real DHCP ACK, DNS publication, stopped-producer expiry and journal recovery passed")
	}
}

func migratedDHCPDNSFixtureDocument(t *testing.T, dnsPort int) config.Document {
	t.Helper()
	input := fmt.Sprintf(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: legacy-dhcp-dns-fixture}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: lan}
      spec: {ifname: lan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: gateway}
      spec: {interface: lan, address: 192.0.2.1/24}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Server
      metadata: {name: leases}
      spec:
        interface: lan
        addressPool: {start: 192.0.2.100, end: 192.0.2.102, leaseTime: 60s}
        gateway: 192.0.2.1
        dnsServers: [192.0.2.1]
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSResolver
      metadata: {name: dns}
      spec:
        listen: [{name: local, addresses: [127.0.0.1], port: %d, sources: [local-zone]}]
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSZone
      metadata: {name: lan-zone}
      spec:
        zone: lab.example
        records: [{hostname: router, ipv4: 192.0.2.1}]
        dhcpDerived: {sources: [DHCPv4Server/leases]}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSForwarder
      metadata: {name: local-zone}
      spec: {resolver: DNSResolver/dns, match: [lab.example], zoneRefs: [DNSZone/lan-zone]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: isolated-lan}
      spec: {role: untrust, interfaces: [lan]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: explicit-dhcp-deny}
      spec: {fromZone: isolated-lan, toZone: self, protocol: udp, port: 67, action: drop}
`, dnsPort)
	consent := true
	result, err := legacyconfig.CompileWithOptions([]byte(input), legacyconfig.Options{
		SchemaVersion: legacyconfig.OptionsSchemaVersion,
		DHCPDNS:       map[string]legacyconfig.DHCPDNSPolicy{"DNSResolver/dns": {SourceScopedFiniteLeases: &consent}},
	})
	if err != nil {
		t.Fatal("compile legacy DHCP/DNS fixture", err)
	}
	return result.Document
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
	fixtureName := os.Getenv("ROUTERD2_PROTOCOL_FIXTURE")
	knownFixture := fixtureName == "TestDaemonDHCPDNSAndOwnershipExpiry" || fixtureName == "TestDaemonMigratedDHCPDNSAndOwnershipExpiry"
	if err != nil || current != expected || current == os.Getenv("ROUTERD2_DAEMON_DHCP_SERVER") || current == os.Getenv("ROUTERD2_PROTOCOL_PARENT") || !knownFixture {
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
