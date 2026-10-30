//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/imksoo/routerd2/internal/retention"
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
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}},
	}
	work := t.TempDir()
	f := startFixtureDaemonAt(t, config.FromNative(native), work)
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
	historyAvailable := false
	history := func() []logstore.DHCPFingerprint {
		t.Helper()
		db, err := logstore.OpenDHCPFingerprintLogReadOnly(historyPath)
		if err != nil {
			if historyAvailable {
				t.Fatal("read previously created fingerprint history", err)
			}
			return nil
		}
		defer db.Close()
		rows, err := db.List(t.Context(), logstore.DHCPFingerprintFilter{Limit: 100})
		if err != nil {
			if historyAvailable {
				t.Fatal("query previously created fingerprint history", err)
			}
			return nil
		}
		historyAvailable = true
		return rows
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
		if rows := history(); len(rows) != 0 {
			t.Fatal("fingerprint history continued after collection was removed", rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("removing fingerprint retention stopped collection while a new MAC still acquired DHCP and reached its gateway")
}
