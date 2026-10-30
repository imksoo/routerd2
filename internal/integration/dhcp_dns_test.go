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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/network"
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
	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true,
		Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", LeaseSeconds: 60, Options: []dhcpserverconfig.Option{{Name: "dns-server", Value: "192.0.2.1"}}},
	}
	dns := dnsconfig.RuntimeConfig{Resource: "dns",
		Spec:  api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: dnsPort}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example", "2.0.192.in-addr.arpa"}, ZoneRef: []string{"DNSZone/lan"}}}},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lab.example", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: "192.0.2.1"}}, DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/leases"}}}}},
	}
	native := config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}, {Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: dns}}},
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
	type clientLease struct {
		CurrentAddress   string
		ServerID         string
		LeaseTimeSeconds int64
		DNS              string
	}
	script := filepath.Join(work, "lease.sh")
	report := filepath.Join(work, "lease.json")
	// Apply only the ACK-provided address and mask to the independent client.
	// DHCPRELEASE uses this source address and the resulting connected route.
	body := "#!/bin/sh\ncase \"$1\" in bound|renew) ip -4 addr replace \"$ip/$subnet\" dev \"$interface\" || exit 1; printf '{\"CurrentAddress\":\"%s\",\"ServerID\":\"%s\",\"LeaseTimeSeconds\":%s,\"DNS\":\"%s\"}\\n' \"$ip\" \"$serverid\" \"$lease\" \"$dns\" > " + report + ";; esac\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	requested := ""
	acquire := func(ifname, hostname string, release bool) clientLease {
		t.Helper()
		args := []string{"udhcpc", "-f", "-n", "-i", ifname, "-x", "hostname:" + hostname, "-s", script}
		if release {
			args = append(args, "-R")
		} else {
			args = append(args, "-q")
		}
		if ifname == "client0" && requested != "" {
			args = append(args, "-r", requested)
		}
		cmd := exec.Command("busybox", args...)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := os.Remove(report); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := fixtureInsideNamespace(clientNS, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if release {
			// BusyBox -q -R exits before entering BOUND and sends no RELEASE.
			// Let the normal event loop enter BOUND, then terminate with -R.
			defer cmd.Process.Kill()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(report); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("Release client did not acquire a lease: %s", output.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("independent BusyBox DHCP client: %v\n%s", err, output.String())
		}
		if release {
			t.Logf("real Release client: %s", output.String())
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var lease clientLease
		if err := json.Unmarshal(data, &lease); err != nil {
			t.Fatal(err)
		}
		if ifname == "client0" {
			requested = lease.CurrentAddress
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
	lease := acquire("client0", "workstation", false)
	if (lease.CurrentAddress != "192.0.2.100" && lease.CurrentAddress != "192.0.2.101" && lease.CurrentAddress != "192.0.2.102") || lease.ServerID != "192.0.2.1" || lease.LeaseTimeSeconds <= 0 || lease.DNS != "192.0.2.1" {
		t.Fatalf("unexpected DHCP lease: %+v", lease)
	}
	if migrated {
		// A second MAC behind the same LAN exercises the deployed special-device
		// profile without manually assigning an address to either client.
		for _, args := range [][]string{
			{"link", "add", "special0", "link", "client0", "address", "02:bb:00:00:00:01", "type", "macvlan", "mode", "bridge"},
			{"link", "set", "special0", "up"},
		} {
			cmd := exec.Command("ip", args...)
			if err := fixtureInsideNamespace(clientNS, cmd.Run); err != nil {
				t.Fatal(err)
			}
		}
		special := acquire("special0", "special0", false)
		if (special.CurrentAddress != "198.51.100.160" && special.CurrentAddress != "198.51.100.161") || special.DNS != "198.51.100.129" {
			t.Fatalf("special client did not receive its own subnet and DNS: %+v", special)
		}
		for _, args := range [][]string{
			{"link", "add", "special1", "link", "client0", "address", "02:bb:00:00:00:02", "type", "macvlan", "mode", "bridge"},
			{"link", "set", "special1", "up"},
		} {
			cmd := exec.Command("ip", args...)
			if err := fixtureInsideNamespace(clientNS, cmd.Run); err != nil {
				t.Fatal(err)
			}
		}
		other := acquire("special1", "special1", false)
		if (other.CurrentAddress != "198.51.100.160" && other.CurrentAddress != "198.51.100.161") || other.CurrentAddress == special.CurrentAddress || other.DNS != special.DNS {
			t.Fatalf("device selectors sharing a pool did not allocate distinct addresses: %+v %+v", special, other)
		}
		t.Log("ordinary and special clients acquired distinct configured subnets and DNS; two device selectors shared a pool without duplicate allocation")
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
		if r.Rcode != mdns.RcodeSuccess && r.Rcode != mdns.RcodeNameError {
			t.Fatalf("DNS failure cannot establish lease withdrawal: %s", r)
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
	if err := json.Unmarshal(expired.Snapshot, &original); err != nil || !expired.OwnershipExpired || !original.Active {
		t.Fatal("current DNS eligibility and the publisher's last observation were not distinguished", err, expired, original)
	}
	leaseStillValid := false
	for _, saved := range original.Leases {
		if saved.IPAddress == lease.CurrentAddress && time.Now().Before(saved.ExpiresAt) {
			leaseStillValid = true
		}
	}
	if !leaseStillValid {
		t.Fatal("ownership expiry was not distinguished from the tested address lease deadline")
	}
	if err := syscall.Kill(serverPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait(8*time.Second, func() bool {
		s := status()
		return pid(s, serverName) != 0 && pid(s, serverName) != serverPID && hasAddress("workstation.lab.example.", lease.CurrentAddress)
	})
	// The independent client requests its previous address after server restart.
	// The server must retain the binding and the DNS process must stay running.
	recovered := acquire("client0", "workstation", false)
	if recovered.CurrentAddress != lease.CurrentAddress || pid(status(), dnsName) != dnsPID {
		t.Fatal("DHCP/DNS restart did not preserve the lease and unaffected worker")
	}
	if !hasAddress("workstation.lab.example.", lease.CurrentAddress) {
		t.Fatal("renewed lease disappeared from the existing DNS TCP connection")
	}
	t.Log("real DHCP ACK and persistent DNS TCP: original boot deadline removed DNS answers while server, control and supervisor were paused; journal recovery restored the lease on the same DNS connection")
	// The same real client changes its name without changing its MAC or binding.
	renamed := acquire("client0", "renamed-workstation", false)
	if renamed.CurrentAddress != recovered.CurrentAddress {
		t.Fatal("hostname change lost the same client's DHCP binding")
	}
	name := "renamed-workstation.lab.example."
	reverse, err := mdns.ReverseAddr(renamed.CurrentAddress)
	if err != nil {
		t.Fatal(err)
	}
	var newA, oldA, newPTR []mdns.RR
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last hostname-change answers: new A=%v old A=%v PTR=%v", newA, oldA, newPTR)
			var input dnsconfig.Input
			err := f.control.Do(context.Background(), "GET", "/v1/services/"+dnsName+"/dns-input", nil, &input)
			t.Logf("last DHCP DNS sources: err=%v sets=%+v", err, input.Sets)
		}
	})
	wait(5*time.Second, func() bool {
		newA = query(name, mdns.TypeA)
		oldA = query("workstation.lab.example.", mdns.TypeA)
		newPTR = query(reverse, mdns.TypePTR)
		if len(newA) != 1 || len(oldA) != 0 {
			return false
		}
		address, ok := newA[0].(*mdns.A)
		if !ok || address.A.String() != renamed.CurrentAddress {
			return false
		}
		for _, rr := range newPTR {
			if ptr, ok := rr.(*mdns.PTR); ok && ptr.Ptr == name {
				return true
			}
		}
		return false
	})
	t.Logf("hostname change: A=%v old A=%v PTR=%v", newA, oldA, newPTR)
	for _, check := range []struct{ name, kind, want string }{
		{name, "A", renamed.CurrentAddress},
		{reverse, "PTR", name},
	} {
		answer, err := exec.Command("dig", "@127.0.0.1", "-p", fmt.Sprint(dnsPort), check.name, check.kind, "+short", "+time=2", "+tries=1").CombinedOutput()
		if err != nil || strings.TrimSpace(string(answer)) != check.want {
			t.Fatalf("independent dig %s %s: %v\n%s", check.name, check.kind, err, answer)
		}
	}
	// Only this final acquisition sends a real DHCPRELEASE when udhcpc exits.
	released := acquire("client0", "renamed-workstation", true)
	if released.CurrentAddress != renamed.CurrentAddress {
		t.Fatal("release client acquired a different DHCP binding")
	}
	wait(5*time.Second, func() bool {
		return len(query(name, mdns.TypeA)) == 0 && len(query(reverse, mdns.TypePTR)) == 0
	})
	if !hasAddress("router.lab.example.", "192.0.2.1") || pid(status(), dnsName) != dnsPID {
		t.Fatal("DHCP hostname update or Release interrupted static DNS or replaced its TCP listener")
	}
	t.Log("same-MAC BusyBox hostname change replaced A/PTR and removed the old name; independent dig agreed; real Release removed A/PTR while static DNS and the original TCP connection survived")
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
      kind: IPv4StaticAddress
      metadata: {name: special-subnet}
      spec: {interface: lan, address: 198.51.100.129/25}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Server
      metadata: {name: leases}
      spec:
        interface: lan
        logDHCP: true
        addressPool: {start: 192.0.2.100, end: 192.0.2.102, leaseTime: 60s}
        gateway: 192.0.2.1
        dnsServers: [192.0.2.1]
        scopes:
          - name: special
            match: {macAddresses: ["02:bb:00:00:00:01"]}
            addressPool: {start: 198.51.100.160, end: 198.51.100.161, leaseTime: 60s}
          - name: special-other
            match: {macAddresses: ["02:bb:00:00:00:02"]}
            addressPool: {start: 198.51.100.160, end: 198.51.100.161, leaseTime: 60s}
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
        dhcpDerived: {sources: [DHCPv4Server/leases], leaseFile: /var/lib/routerd/dnsmasq/dnsmasq.leases, ddns: true, ttl: 60}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSForwarder
      metadata: {name: local-zone}
      spec: {resolver: DNSResolver/dns, match: [lab.example, 2.0.192.in-addr.arpa], zoneRefs: [DNSZone/lan-zone]}
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
