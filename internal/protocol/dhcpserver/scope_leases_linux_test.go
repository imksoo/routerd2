// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

func TestPruneScopeMismatchedLeases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.leases")
	cfg := Config{
		Resource:      "leases",
		Interface:     "lan0",
		ServerAddress: "192.0.2.1",
		StatePath:     path,
		Primary:       Scope{Name: "default", Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", LeaseSeconds: 120},
		Scopes: []Scope{{
			Name: "special", Prefix: "198.51.100.128/25", Start: "198.51.100.160", End: "198.51.100.161", LeaseSeconds: 120,
			Match: dhcpserverconfig.Match{OUIPrefixes: []string{"02:bb:00"}},
		}},
		Reservations: []Reservation{{MACAddress: "02:bb:00:00:00:04", IPAddress: "192.0.2.101"}},
	}
	data := strings.Join([]string{
		"1760000000 02:bb:00:00:00:01 192.0.2.100 old-selected *",
		"1760000000 02:bb:00:00:00:02 198.51.100.160 selected *",
		"1760000000 02:aa:00:00:00:03 192.0.2.101 ordinary *",
		"1760000000 02:bb:00:00:00:04 192.0.2.101 reserved *",
		"duid 00:01:00:01:00:00:00:00:00:00:00:00:00:00",
		"broken lease line",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneScopeMismatchedLeases(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "old-selected") {
		t.Fatalf("old selected-pool lease remains:\n%s", got)
	}
	for _, want := range []string{"selected", "ordinary", "reserved", "duid", "broken lease line"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("expected %q to remain:\n%s", want, got)
		}
	}
	if err := pruneScopeMismatchedLeases(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("second prune changed lease state:\n%s", again)
	}
	if err := saveTransfer(context.Background(), cfg, leaseTransfer{Leases: data}); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(merged), "old-selected") {
		t.Fatalf("peer transfer restored the old selected-pool lease:\n%s", merged)
	}
}
