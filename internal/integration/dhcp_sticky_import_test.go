//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
)

func TestDaemonImportedDHCPStickyHistory(t *testing.T) {
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
	events, serverHook := filepath.Join(work, "old.events"), filepath.Join(work, "old-hook.sh")
	if err := os.WriteFile(serverHook, []byte(fmt.Sprintf("#!/bin/sh\nset -eu\ncase \"$1\" in add|old|del) printf '%%s %%s %%s %%s %%s\\n' \"$(date +%%s%%N)\" \"$1\" \"$2\" \"$3\" \"${4-}\" >> %q;; esac\n", events)), 0700); err != nil {
		t.Fatal(err)
	}
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
		"--dhcp-script="+serverHook, "--dhcp-leasefile="+oldLeases, "--dhcp-range=192.0.2.100,192.0.2.102,255.255.255.0,1200s",
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
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nset -eu\ncase \"$1\" in bound|renew) ip -4 addr replace \"$ip/$subnet\" dev \"$interface\"; printf '%s %s %s %s\\n' \"$ip\" \"$router\" \"$dns\" \"$subnet\" > "+report+";; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	acquire := func(iface, hostname, requested string, release bool) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		args := []string{"udhcpc", "-f", "-n", "-t", "5", "-T", "1", "-i", iface, "-x", "hostname:" + hostname, "-s", hook}
		if !release {
			args = append(args, "-q")
		}
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
		if release {
			until(t, 8*time.Second, func() bool {
				content, _ := os.ReadFile(report)
				return len(strings.Fields(string(content))) == 4
			})
			if err := command.Process.Signal(syscall.SIGUSR2); err != nil {
				t.Fatal(err)
			}
			until(t, 3*time.Second, func() bool {
				content, _ := os.ReadFile(events)
				return strings.Contains(string(content), " del ") && strings.HasSuffix(string(content), "\n")
			})
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		if err := command.Wait(); err != nil {
			t.Fatalf("independent DHCP client %s: %v\n%s", iface, err, output.String())
		}
		content, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(content))
		if len(fields) != 4 || net.ParseIP(fields[0]) == nil || fields[1] != "192.0.2.1" || fields[2] != "192.0.2.1" {
			t.Fatalf("unexpected acquired address/router/DNS: %s", content)
		}
		t.Logf("independent client %s: address=%s router=%s DNS=%s", hostname, fields[0], fields[1], fields[2])
		return fields
	}
	original := acquire("client0", "migrated-client", "", true)
	// These are actual dnsmasq script notifications, including the client's
	// DHCPRELEASE. Use their captured times with the old routerd writer.
	notifications, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := old.Wait(); err != nil {
		t.Fatal("stop old DHCP server", err)
	}
	oldDatabase := filepath.Join(work, "old-sticky.db")
	oldHistory, err := logstore.OpenDHCPStickyLog(oldDatabase)
	if err != nil {
		t.Fatal(err)
	}
	defer oldHistory.Close()
	added, released := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(notifications)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[3] != original[0] {
			t.Fatalf("unexpected independent dnsmasq notification: %q", line)
		}
		nanos, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		action := ""
		switch fields[1] {
		case "add":
			action, added = "added", true
		case "old":
			action = "renewed"
		case "del":
			action, released = "removed", true
		}
		if err := oldHistory.RecordLeaseEvent(t.Context(), action, fields[2], fields[3], fields[4], 3, time.Unix(0, nanos)); err != nil {
			t.Fatal(err)
		}
	}
	if err := oldHistory.Close(); err != nil {
		t.Fatal(err)
	}
	if !added || !released {
		t.Fatal("independent server did not observe both allocation and release")
	}
	readHistory := func(path string) logstore.DHCPStickyLease {
		t.Helper()
		db, err := logstore.OpenDHCPStickyLogReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.List(t.Context(), logstore.DHCPStickyFilter{IP: original[0], Now: time.Now(), Limit: 10})
		if err != nil || len(rows) != 1 {
			t.Fatalf("sticky history: %v, rows=%v", err, rows)
		}
		return rows[0]
	}
	retained := readHistory(oldDatabase)
	if retained.ReleasedAt.IsZero() || !retained.StickyUntil.Equal(retained.ReleasedAt.Add(3*24*time.Hour)) {
		t.Fatal("actual release did not produce the original three-day hold", retained)
	}
	// Remove the released address before the other MAC asks for it. Otherwise
	// dnsmasq's ARP conflict detection could conceal missing sticky protection.
	if err := fixtureInsideNamespace(peer, func() error {
		return exec.Command("ip", "-4", "addr", "del", original[0]+"/"+original[3], "dev", "client0").Run()
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("old dnsmasq observed acquisition and RELEASE; original allocation=%s release=%s hold=%s", retained.AllocatedAt.Format(time.RFC3339Nano), retained.ReleasedAt.Format(time.RFC3339Nano), retained.StickyUntil.Format(time.RFC3339Nano))
	if err := netlink.AddrDel(link, address); err != nil {
		t.Fatal(err)
	}

	if err := netlink.LinkSetDown(link); err != nil {
		t.Fatal(err)
	}

	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true, StickyHoldDays: 3,
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
	command := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "dhcp", "import-sticky",
		"--config", configPath, "--service", "leases", "--state-dir", stateDir, oldDatabase)
	imported, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("product sticky import: %v\n%s", err, imported)
	}
	var result struct {
		Service   string `json:"service"`
		Records   int    `json:"records"`
		StatePath string `json:"state_path"`
	}
	if err := json.Unmarshal(imported, &result); err != nil || result.Service != "leases" || result.Records != 1 {
		t.Fatalf("product sticky import result: %v %s", err, imported)
	}
	if actual := readHistory(result.StatePath); actual != retained {
		t.Fatal("import changed the original sticky record or deadline", retained, actual)
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
	for _, args := range [][]string{
		{"link", "add", "other0", "link", "client0", "address", "02:bb:00:00:01:02", "type", "macvlan", "mode", "bridge"},
		{"link", "set", "other0", "up"},
	} {
		command := exec.Command("ip", args...)
		if err := fixtureInsideNamespace(peer, command.Run); err != nil {
			t.Fatal(err)
		}
	}
	other := acquire("other0", "new-client", original[0], false)
	if other[0] == original[0] {
		t.Fatal("new client received the imported client's released but held address")
	}
	if actual := readHistory(result.StatePath); actual != retained {
		t.Fatal("another client changed the original sticky record or deadline", retained, actual)
	}
	renewed := acquire("client0", "migrated-client", original[0], false)
	if renewed[0] != original[0] {
		t.Fatal("original client lost its imported address", original, renewed)
	}
	until(t, 5*time.Second, func() bool { return clientDNS("+notcp") })
	if !clientDNS("+tcp") {
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
	t.Logf("PASS independent dnsmasq acquisition/release -> old RecordLeaseEvent with actual timestamps -> server stop -> product CLI sticky import with original deadlines -> distinct new client %s -> original MAC reclaims %s; acquired DNS UDP/TCP and IPv4 gateway traffic", other[0], renewed[0])
}
