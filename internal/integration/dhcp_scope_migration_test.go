//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
)

func TestDaemonDHCPSelectorScopeMigration(t *testing.T) {
	if !isolateFixtureTimeout(t, t.Name(), 70*time.Second) {
		return
	}
	clientNS := fixturePeerNamespace(t, "lan0", "client0")
	if err := fixtureInsideNamespace(clientNS, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		return netlink.LinkSetHardwareAddr(link, net.HardwareAddr{0x02, 0xbb, 0, 0, 0, 1})
	}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Chmod(work, 0700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(work, "lease.json")
	script := filepath.Join(work, "lease.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in bound|renew) ip -4 addr replace \"$ip/$subnet\" dev \"$interface\" || exit 1; printf '{\"address\":\"%s\",\"server\":\"%s\"}\\n' \"$ip\" \"$serverid\" > "+report+";; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	type lease struct {
		Address string `json:"address"`
		Server  string `json:"server"`
	}
	acquire := func(requested string) lease {
		t.Helper()
		if err := os.Remove(report); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		args := []string{"udhcpc", "-f", "-n", "-q", "-i", "client0", "-x", "hostname:scope-client", "-s", script}
		if requested != "" {
			args = append(args, "-r", requested)
		}
		cmd := exec.Command("busybox", args...)
		if err := fixtureInsideNamespace(clientNS, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var got lease
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	document := func(scopes []dhcpserverconfig.Scope) config.Document {
		server := serviceconfig.DHCPServer{
			Interface: "lan0", ServerAddress: "192.0.2.1", Authoritative: true,
			Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", LeaseSeconds: 120},
			Scopes:  scopes,
		}
		return config.FromNative(config.Native{
			Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24", "198.51.100.129/25"}}}},
			Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}},
		})
	}
	first := startFixtureDaemonAt(t, document(nil), work)
	defer first.stop()
	old := acquire("")
	if !strings.HasPrefix(old.Address, "192.0.2.") || old.Server != "192.0.2.1" {
		t.Fatalf("initial unscoped lease = %+v", old)
	}
	state := filepath.Join(work, "state", "services", "leases", "dnsmasq.leases")
	before, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(before), " "+old.Address+" ") {
		t.Fatalf("initial dynamic lease was not persisted: %v\n%s", err, before)
	}
	selected := document([]dhcpserverconfig.Scope{{
		Name: "special", Prefix: "198.51.100.128/25", Start: "198.51.100.160", End: "198.51.100.161", Router: "198.51.100.129", LeaseSeconds: 120,
		Match: dhcpserverconfig.Match{MACAddresses: []string{"02:bb:00:00:00:01"}},
	}})
	if err := first.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "selected-scope-migration", Config: selected}, nil); err != nil {
		t.Fatal(err)
	}
	migrated := acquire(old.Address)
	if migrated.Address != "198.51.100.160" && migrated.Address != "198.51.100.161" || migrated.Server != "192.0.2.1" {
		t.Fatalf("selected client retained an address outside its new scope: old=%+v migrated=%+v", old, migrated)
	}
	after, err := os.ReadFile(state)
	if err != nil || strings.Contains(string(after), " "+old.Address+" ") {
		t.Fatalf("old selected-pool lease survived migration: %v\n%s", err, after)
	}
	t.Logf("real client migrated from %s to selected pool %s after worker restart", old.Address, migrated.Address)
}
