//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
)

// Exercise the imported flag through dnsmasq stderr and the product log API.
// An ACK alone does not prove log-dhcp: requested-option details require it.
func TestDaemonMigratedDHCPLogs(t *testing.T) {
	if !isolateFixture(t, "TestDaemonMigratedDHCPLogs") {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "client0")
	doc := migratedDHCPDNSFixtureDocument(t, port(t))
	work := t.TempDir()
	startFixtureDaemonAt(t, doc, work)
	logs := localapi.NewClient(filepath.Join(work, "run", logging.SocketName))
	report := filepath.Join(work, "lease.txt")
	script := filepath.Join(work, "lease.sh")
	hook := fmt.Sprintf(`#!/bin/sh
case "$1" in
bound|renew)
  busybox ifconfig "$interface" "$ip" netmask "$subnet"
  printf '%%s %%s\n' "$1" "$ip" >> %s
  ;;
esac
`, report)
	if err := os.WriteFile(script, []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	client := exec.Command("busybox", "udhcpc", "-f", "-i", "client0", "-x", "hostname:log-client", "-s", script, "-p", filepath.Join(work, "client.pid"))
	output, err := os.Create(filepath.Join(work, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	client.Stdout, client.Stderr = output, output
	if err := fixtureInsideNamespace(peer, client.Start); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Process.Kill()
		_ = client.Wait()
	}()
	var address string
	until(t, 10*time.Second, func() bool {
		data, _ := os.ReadFile(report)
		fields := strings.Fields(string(data))
		if len(fields) != 2 || fields[0] != "bound" {
			return false
		}
		address = fields[1]
		return true
	})
	query := func(after logging.Cursor) logging.QueryResult {
		t.Helper()
		path := "/v1/logs?limit=200"
		if after.Epoch != "" {
			path += fmt.Sprintf("&epoch=%s&after=%d", after.Epoch, after.Sequence)
		}
		var result logging.QueryResult
		if err := logs.Do(context.Background(), "GET", path, nil, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	var cursor logging.Cursor
	var module string
	until(t, 5*time.Second, func() bool {
		result := query(logging.Cursor{})
		var ack, details bool
		for _, record := range result.Records {
			if strings.Contains(record.Message, "DHCPACK(lan0) "+address) {
				ack, module = true, record.Module
			}
			details = details || strings.Contains(record.Message, "requested options:")
		}
		cursor = result.Cursor
		return ack && details
	})
	if !strings.HasPrefix(module, "service.") {
		t.Fatal("DHCP log missing its service identity", module)
	}
	if err := client.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(report)
		return strings.Contains(string(data), "renew "+address+"\n")
	})
	var records []logging.Record
	until(t, 5*time.Second, func() bool {
		result := query(cursor)
		var request, ack bool
		records = nil
		for _, record := range result.Records {
			if record.Module != module {
				continue
			}
			request = request || strings.Contains(record.Message, "DHCPREQUEST(lan0) "+address)
			ack = ack || strings.Contains(record.Message, "DHCPACK(lan0) "+address)
			records = append(records, record)
		}
		return request && ack
	})
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("product log: %s", data)
	}
	t.Logf("imported logDHCP: true; independent BusyBox bound and renewed %s; detailed options, request and ACK reached %s with product sequence and process identity", address, module)
}
