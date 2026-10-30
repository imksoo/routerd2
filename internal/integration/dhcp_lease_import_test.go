//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
)

func TestDaemonImportedDHCPLeases(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "client0")
	work := t.TempDir()
	link, err := netlink.LinkByName("lan0")
	if err != nil {
		t.Fatal(err)
	}
	address, err := netlink.ParseAddr("192.0.2.1/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	oldLeases := filepath.Join(work, "old.leases")
	oldLog, err := os.Create(filepath.Join(work, "old-dnsmasq.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		oldLog.Close()
		if t.Failed() {
			content, _ := os.ReadFile(oldLog.Name())
			t.Logf("independent dnsmasq: %s", content)
		}
	})
	old := exec.Command("dnsmasq", "--no-daemon", "--conf-file=/dev/null", "--interface=lan0",
		"--bind-interfaces", "--port=0", "--dhcp-authoritative", "--log-dhcp", "--pid-file=",
		"--dhcp-leasefile="+oldLeases, "--dhcp-range=192.0.2.100,192.0.2.102,255.255.255.0,1200s",
		"--dhcp-option=option:router,192.0.2.1", "--dhcp-option=option:dns-server,192.0.2.1")
	old.Stdout, old.Stderr = oldLog, oldLog
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if old.ProcessState == nil {
			old.Process.Kill()
			old.Wait()
		}
	})

	report, hook := filepath.Join(work, "client.report"), filepath.Join(work, "client.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nset -eu\ncase \"$1\" in bound|renew) ip -4 addr replace \"$ip/$subnet\" dev \"$interface\"; printf '%s %s %s\\n' \"$ip\" \"$router\" \"$dns\" > "+report+";; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	acquire := func(iface, hostname, requested string) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		args := []string{"udhcpc", "-f", "-n", "-q", "-t", "5", "-T", "1", "-i", iface, "-x", "hostname:" + hostname, "-s", hook}
		if requested != "" {
			args = append(args, "-r", requested)
		}
		command := exec.CommandContext(ctx, "busybox", args...)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := os.Remove(report); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := fixtureInsideNamespace(peer, command.Start); err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatalf("independent DHCP client %s: %v\n%s", iface, err, output.String())
		}
		content, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(content))
		if len(fields) != 3 || net.ParseIP(fields[0]) == nil || fields[1] != "192.0.2.1" || fields[2] != "192.0.2.1" {
			t.Fatalf("unexpected acquired address/router/DNS: %s", content)
		}
		t.Logf("independent client %s: address=%s router=%s DNS=%s", hostname, fields[0], fields[1], fields[2])
		return fields
	}
	original := acquire("client0", "migrated-client", "")
	if err := old.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := old.Wait(); err != nil {
		t.Fatal("stop old DHCP server", err)
	}
	leaseBytes, err := os.ReadFile(oldLeases)
	if err != nil || !strings.Contains(string(leaseBytes), original[0]+" migrated-client ") {
		t.Fatalf("independent server did not persist the acquired binding: %v %s", err, leaseBytes)
	}
	t.Logf("old server exited; original dnsmasq lease: %s", leaseBytes)
	if err := netlink.AddrDel(link, address); err != nil {
		t.Fatal(err)
	}

	if err := netlink.LinkSetDown(link); err != nil {
		t.Fatal(err)
	}

	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true,
		Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", LeaseSeconds: 1200, Options: []dhcpserverconfig.Option{{Name: "dns-server", Value: "192.0.2.1"}}}}
	dns := dnsconfig.RuntimeConfig{Resource: "dns",
		Spec:  api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "lan", Addresses: []string{"192.0.2.1"}, Port: 53}}, Sources: []api.DNSResolverSourceSpec{{Name: "zone", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"DNSZone/lan"}}}},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lab.example", DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/leases"}}}}}}
	doc := config.FromNative(config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}, {Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: dns}}},
	})
	configPath, stateDir := filepath.Join(work, "config.json"), filepath.Join(work, "state")
	if err := config.AtomicJSON(configPath, doc); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "dhcp", "import-leases",
		"--config", configPath, "--service", "leases", "--state-dir", stateDir, oldLeases)
	imported, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("product lease import: %v\n%s", err, imported)
	}
	t.Logf("product import response: %s", imported)
	startFixtureDaemonAt(t, doc, work)
	clientDNS := func(transport string) bool {
		t.Helper()
		var answer []byte
		err := fixtureInsideNamespace(peer, func() error {
			var err error
			answer, err = exec.Command("dig", "@"+original[2], "migrated-client.lab.example.", "A", "+short", "+time=1", "+tries=1", transport).CombinedOutput()
			return err
		})
		return err == nil && strings.TrimSpace(string(answer)) == original[0]
	}
	// Before another DHCP request, the old client's still-valid lease must
	// already supply DNS. Reallocation by the same engine hash cannot fake this.
	until(t, 5*time.Second, func() bool { return clientDNS("+notcp") })
	if !clientDNS("+tcp") {
		t.Fatal("imported binding was not available through the acquired TCP DNS server")
	}
	for _, args := range [][]string{
		{"link", "add", "other0", "link", "client0", "address", "02:bb:00:00:01:02", "type", "macvlan", "mode", "bridge"},
		{"link", "set", "other0", "up"},
	} {
		command := exec.Command("ip", args...)
		if err := fixtureInsideNamespace(peer, command.Run); err != nil {
			t.Fatal(err)
		}
	}
	other := acquire("other0", "new-client", original[0])
	if other[0] == original[0] {
		t.Fatal("new client received the imported client's unexpired address")
	}
	renewed := acquire("client0", "migrated-client", original[0])
	if renewed[0] != original[0] {
		t.Fatal("original client lost its imported address", original, renewed)
	}
	if !clientDNS("+notcp") || !clientDNS("+tcp") {
		t.Fatal("renewed client could not use DHCP-provided DNS")
	}
	for _, client := range []struct{ iface, gateway string }{{"client0", renewed[1]}, {"other0", other[1]}} {
		var output []byte
		err := fixtureInsideNamespace(peer, func() error {
			var err error
			output, err = exec.Command("busybox", "ping", "-I", client.iface, "-c", "1", "-W", "2", client.gateway).CombinedOutput()
			return err
		})
		if err != nil {
			t.Fatalf("IPv4 communication with acquired lease on %s: %v\n%s", client.iface, err, output)
		}
	}
	t.Logf("PASS independent dnsmasq stop -> product CLI import -> imported DNS before DHCP -> distinct new client %s -> original address %s retained; acquired DNS UDP/TCP and IPv4 gateway traffic", other[0], renewed[0])
}
