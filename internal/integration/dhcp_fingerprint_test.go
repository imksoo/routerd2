//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/retention"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestDaemonDHCPFingerprints(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "client0")
	clients := []struct{ iface, mac string }{
		{"finger0", "02:bb:00:00:01:01"},
		{"finger1", "02:bb:00:00:01:02"},
		{"finger2", "02:bb:00:00:01:03"},
	}
	for _, client := range clients {
		for _, args := range [][]string{
			{"link", "add", client.iface, "link", "client0", "address", client.mac, "type", "macvlan", "mode", "bridge"},
			{"link", "set", client.iface, "up"},
		} {
			cmd := exec.Command("ip", args...)
			if err := fixtureInsideNamespace(peer, cmd.Run); err != nil {
				t.Fatal(err)
			}
		}
	}
	server := serviceconfig.DHCPServer{
		Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true,
		Primary:              dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.110", Router: "192.0.2.1", LeaseSeconds: 120},
		FingerprintRetention: &retention.Config{MaxAgeSeconds: 12, IntervalSeconds: 1},
	}
	native := config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server},
			{Name: "console", WebConsole: &webconsoleconfig.Config{Listen: netip.MustParseAddrPort("192.0.2.1:8080")}}},
	}
	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	clientKey, hostKey := filepath.Join(work, "client_key"), filepath.Join(stateDir, "ssh_host_ed25519_key")
	for _, path := range []string{clientKey, hostKey} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("independent SSH key: %v %s", err, output)
		}
	}
	clientPublic, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	sshPort := port(t)
	knownHosts := filepath.Join(work, "known_hosts")
	if err := os.WriteFile(knownHosts, append([]byte(fmt.Sprintf("[192.0.2.1]:%d ", sshPort)), hostPublic...), 0600); err != nil {
		t.Fatal(err)
	}
	f := startFixtureDaemonConfigured(t, config.FromNative(native), work, nil, runtimeconfig.SSH{
		Listen: fmt.Sprintf("192.0.2.1:%d", sshPort), User: "admin", AuthorizedKeys: []string{string(clientPublic)},
	})
	sshRead := func(command, input string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		args := []string{"-T", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=3", "-i", clientKey, "-p", fmt.Sprint(sshPort), "admin@192.0.2.1"}
		if command != "" {
			args = append(args, command)
		}
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdin = strings.NewReader(input)
		var output []byte
		if err := fixtureInsideNamespace(peer, func() error { var err error; output, err = cmd.CombinedOutput(); return err }); err != nil {
			t.Fatalf("independent OpenSSH: %v %s", err, output)
		}
		return output
	}
	console := func(query string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "curl", "--fail", "--silent", "--show-error", "--max-time", "4", "http://192.0.2.1:8080/clients?service=service.leases"+query)
		var output []byte
		if err := fixtureInsideNamespace(peer, func() error { var err error; output, err = cmd.CombinedOutput(); return err }); err != nil {
			t.Fatalf("independent HTTP client: %v %s", err, output)
		}
		return string(output)
	}
	historyPath := filepath.Join(work, "state", "services", "leases", "dhcp-fingerprints.db")
	report, script := filepath.Join(work, "lease.txt"), filepath.Join(work, "lease.sh")
	hook := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
bound|renew)
  busybox ifconfig "$interface" "$ip" netmask "$subnet"
  printf '%%s %%s\n' "$ip" "$router" > %s
  ;;
esac
`, report)
	if err := os.WriteFile(script, []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	const vendor = "routerd2-busybox-fixture"
	acquire := func(iface, hostname string) string {
		t.Helper()
		if err := os.Remove(report); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "busybox", "udhcpc", "-f", "-n", "-q", "-i", iface,
			"-x", "hostname:"+hostname, "-V", vendor, "-o", "-O", "1", "-O", "3", "-O", "6", "-O", "15", "-O", "119", "-s", script)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		started := time.Now()
		if err := fixtureInsideNamespace(peer, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("independent BusyBox acquisition on %s: %v\n%s", iface, err, output.String())
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		lease := strings.Fields(string(data))
		if len(lease) != 2 || lease[1] != "192.0.2.1" {
			t.Fatal("client did not receive its configured address and gateway", string(data))
		}
		var host int
		if n, err := fmt.Sscanf(lease[0], "192.0.2.%d", &host); err != nil || n != 1 || host < 100 || host > 110 {
			t.Fatal("client address is outside the configured pool", lease[0])
		}
		output.Reset()
		ping := exec.CommandContext(ctx, "busybox", "ping", "-I", iface, "-c", "1", "-W", "2", lease[1])
		ping.Stdout, ping.Stderr = &output, &output
		if err := fixtureInsideNamespace(peer, ping.Start); err != nil {
			t.Fatal(err)
		}
		if err := ping.Wait(); err != nil {
			t.Fatalf("communication using the DHCP address on %s: %v\n%s", iface, err, output.String())
		}
		t.Logf("independent BusyBox %s acquired %s and reached learned gateway %s in %s", hostname, lease[0], lease[1], time.Since(started))
		return lease[0]
	}
	history := func() []logstore.DHCPFingerprint {
		t.Helper()
		var page runtimeplan.DHCPFingerprintHistory
		if err := f.control.Do(t.Context(), "GET", "/v1/services/service.leases/dhcp-fingerprints", nil, &page); err != nil {
			t.Fatal(err)
		}
		return page.Entries
	}
	recorded := func(mac, hostname string) bool {
		t.Helper()
		for _, row := range history() {
			if row.MAC == mac && row.Hostname == hostname && row.VendorClass == vendor &&
				slices.Equal(row.RequestedOptions, []int{1, 3, 6, 15, 119}) {
				t.Logf("fingerprint: mac=%s hostname=%s vendor=%s requested_options=%v observed=%s",
					row.MAC, row.Hostname, row.VendorClass, row.RequestedOptions, row.ObservedAt.Format(time.RFC3339Nano))
				return true
			}
		}
		return false
	}
	firstAddress := acquire(clients[0].iface, "fingerprint-first")
	until(t, 5*time.Second, func() bool { return recorded(clients[0].mac, "fingerprint-first") })

	// Compare the real DHCP observation across the read-only DB, API, independent
	// OpenSSH command and console. None of those consumers writes history.
	initialRows := history()
	db, err := logstore.OpenDHCPFingerprintLogReadOnly(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.List(t.Context(), logstore.DHCPFingerprintFilter{Limit: 100})
	db.Close()
	if err != nil || !reflect.DeepEqual(initialRows, stored) {
		t.Fatalf("API changed stored fingerprint information: %v", err)
	}
	var sshPage runtimeplan.DHCPFingerprintHistory
	command := "show dhcp-fingerprints leases mac " + strings.ReplaceAll(strings.ToUpper(clients[0].mac), ":", "-") + " since " + initialRows[0].ObservedAt.Format(time.RFC3339Nano) + " limit 1"
	if err := json.Unmarshal(sshRead(command, ""), &sshPage); err != nil || !reflect.DeepEqual(sshPage.Entries, initialRows) || sshPage.Truncated {
		t.Fatalf("SSH read differs from actual DHCP observation: %v %+v", err, sshPage)
	}
	var html string
	// The DHCP notification is persisted before the periodic lease observation
	// reaches control. Wait for that observation, not an arbitrary fixed delay.
	until(t, 5*time.Second, func() bool {
		html = console("&mac=" + url.QueryEscape(clients[0].mac))
		return strings.Contains(html, firstAddress)
	})
	for _, text := range []string{firstAddress, clients[0].mac, vendor, "fingerprint-first", "[1 3 6 15 119]", initialRows[0].Source, "推定結果"} {
		if !strings.Contains(html, text) {
			t.Fatalf("console omitted real lease/fingerprint value %q", text)
		}
	}
	if output := string(sshRead("", "configure\r"+command+"\rabort\rexit\r")); strings.Contains(output, "Error:") || !strings.Contains(output, vendor) {
		t.Fatal("fingerprint query failed in candidate mode", output)
	}
	var unchanged control.State
	if err := f.control.Do(t.Context(), "GET", "/v1/config", nil, &unchanged); err != nil || unchanged.Revision != 1 {
		t.Fatal("read-only fingerprint query changed configuration", err, unchanged.Revision)
	}
	html = console("&limit=0")
	if !strings.Contains(html, "端末識別記録を取得できません") || !strings.Contains(html, firstAddress) {
		t.Fatal("history read error hid current lease information")
	}
	t.Log("read-only API and independent OpenSSH/HTTP preserved actual DHCP observation fields; candidate-mode query left revision unchanged; history error kept lease display")

	// A separate connection owns the write lock while product readers and the
	// independent DHCP client remain live. It never changes history rows.
	lockDB, err := sql.Open("sqlite", historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lockDB.Close()
	lock, err := lockDB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(t.Context(), "PRAGMA busy_timeout = 1000; BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer lock.ExecContext(context.Background(), "ROLLBACK")
	blockedAddress := acquire(clients[1].iface, "fingerprint-locked")
	if blockedAddress == firstAddress {
		t.Fatal("different MACs acquired the same address", blockedAddress)
	}
	logs := localapi.NewClient(filepath.Join(work, "run", logging.SocketName))
	until(t, 8*time.Second, func() bool {
		var records logging.QueryResult
		if err := logs.Do(t.Context(), "GET", "/v1/logs?limit=200", nil, &records); err != nil {
			t.Fatal(err)
		}
		for _, record := range records.Records {
			if strings.Contains(record.Message, "DHCP fingerprint history failed") {
				t.Logf("product reported history failure while the write lock remained held: %s", record.Message)
				return true
			}
		}
		return false
	})
	for _, row := range history() {
		if row.MAC == clients[1].mac {
			t.Fatal("fingerprint was written while another connection held the write lock", row)
		}
	}
	if _, err := lock.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	acquire(clients[1].iface, "fingerprint-recovered")
	until(t, 5*time.Second, func() bool { return recorded(clients[1].mac, "fingerprint-recovered") })
	t.Log("new DHCP acquisition restored fingerprint history after the database write lock was released")

	// The client's process exited after ACK, so no renewal can refresh its age.
	until(t, 16*time.Second, func() bool { return len(history()) == 0 })
	t.Log("configured retention removed real client observations after their age limit")

	acquire(clients[0].iface, "fingerprint-retained")
	until(t, 5*time.Second, func() bool { return recorded(clients[0].mac, "fingerprint-retained") })
	retained := history()
	server.FingerprintRetention = nil
	if err := f.control.Do(t.Context(), "POST", "/v1/config/commit",
		control.CommitRequest{ExpectedRevision: 1, RequestID: "disable-dhcp-fingerprints", Config: config.FromNative(native)}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		var state control.State
		return f.control.Do(t.Context(), "GET", "/v1/config", nil, &state) == nil &&
			state.Revision == 2 && state.Healthy && state.Services != nil &&
			state.Services.Revision == 2 && state.Services.Phase == "ready"
	})
	acquire(clients[2].iface, "fingerprint-disabled")
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if rows := history(); !reflect.DeepEqual(rows, retained) {
			t.Fatal("retained history changed after collection was removed", rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
	html = console("")
	if !strings.Contains(html, "fingerprint-retained") || strings.Contains(html, `data-fingerprint="`+clients[2].mac+`"`) {
		t.Fatal("console did not retain stopped history or invented a new observation")
	}
	t.Log("removing collection preserved original retained observations for API/console while a new MAC acquired DHCP and reached its gateway")
}
