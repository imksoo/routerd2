//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Exercises the real migration -> native service compiler -> public runtime
// ownership boundary. The reused fixture captures network intent in memory and
// serves only a private Unix supervisor socket; it never opens DHCP/DNS ports.
func TestMigratedDHCPDNSGraphThroughPublicRuntimeOwnership(t *testing.T) {
	data := mutateDHCPInput(t, fixture(t, "dhcp-dns"), func(rs []map[string]any) []map[string]any {
		return append(rs,
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "Interface", "metadata": map[string]any{"name": "guest"}, "spec": map[string]any{"ifname": "eth2", "managed": true, "adminUp": true}},
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "IPv4StaticAddress", "metadata": map[string]any{"name": "guest-address"}, "spec": map[string]any{"interface": "guest", "address": "198.51.100.1/24"}},
			map[string]any{"apiVersion": "net.routerd.net/v1alpha1", "kind": "DHCPv4Server", "metadata": map[string]any{"name": "guest-v4"}, "spec": map[string]any{"interface": "guest", "addressPool": map[string]any{"start": "198.51.100.100", "end": "198.51.100.120", "leaseTime": "60s"}}},
		)
	})
	result := compiledPolicy(t, data, dhcpDNSPolicy(t))
	doc := result.Document
	dns := service(t, doc, "dns")
	var dhcp, guest serviceconfig.Service
	for _, s := range nativeProgram(t, doc).Services {
		if s.DHCPServer != nil {
			if s.DHCPServer.Interface == "eth1" {
				dhcp = s
			} else {
				guest = s
			}
		}
	}
	if dhcp.Name == "" || guest.Name == "" {
		t.Fatal("migration fixture requires both named DHCP producers")
	}
	runtime, network, _ := runtimeMigrationFixture(t, doc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initialNetwork := network.state()
	initial, err := runtime.DNSInput(dns.ModuleName())
	if err != nil || initial.Resource != dns.Name || initial.ConfigDigest == "" || len(initial.Sets) != 0 {
		t.Fatal("DNS started with unauthenticated or unfenced lease records", initial, err)
	}
	makeSnapshot := func(s serviceconfig.Service, name, address string, lifetime time.Duration) dhcpserverconfig.Snapshot {
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		now := clock.WallTime
		return dhcpserverconfig.Snapshot{Resource: s.Name, Interface: s.DHCPServer.Interface, ServerAddress: s.DHCPServer.ServerAddress, Epoch: strings.Repeat("b", 32), Sequence: 1, Active: true, ObservedAt: now, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(lifetime),
			Leases: []dhcpserverconfig.Lease{{MACAddress: "02:00:00:00:00:11", IPAddress: address, Hostname: name, ExpiresAt: now.Add(60 * time.Second)}}}
	}
	other := makeSnapshot(guest, "not-authorized", "198.51.100.100", 4*time.Second)
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, guest, 1, other)); err != nil {
		t.Fatal(err)
	}
	projection, err := runtime.DNSInput(dns.ModuleName())
	if err != nil || len(projection.Sets) != 0 {
		t.Fatal("unreferenced DHCP producer leaked across the zone's source allowlist", projection, err)
	}
	active := makeSnapshot(dhcp, "laptop", "192.0.2.100", 300*time.Millisecond)
	update := migrationUpdate(t, runtime, dhcp, 1, active)
	spoof := update
	spoof.ConfigDigest = strings.Repeat("0", len(update.ConfigDigest))
	if err := observeMigration(ctx, runtime, spoof); !errors.Is(err, runtimeplan.ErrStale) {
		t.Fatal("unbound producer digest supplied DNS records", err)
	}
	if err := observeMigration(ctx, runtime, update); err != nil {
		t.Fatal(err)
	}
	projection, err = runtime.DNSInput(dns.ModuleName())
	if err != nil || len(projection.Sets) != 1 || projection.Sets[0].Source != dhcp.Name || projection.Sets[0].Zone != "home" || len(projection.Sets[0].Records) != 1 || projection.Sets[0].Records[0].Hostname != "laptop" || projection.Sets[0].Records[0].Address != "192.0.2.100" {
		t.Fatal("accepted lease did not reach only its configured DNS consumer", projection, err)
	}
	if _, err := runtime.DNSInput(dhcp.ModuleName()); err == nil {
		t.Fatal("DHCP producer obtained a DNS consumer projection")
	}
	// The producer stops publishing while its client lease remains valid. Its
	// short ownership grant must expire independently of the 60-second lease.
	wait := time.NewTimer(310 * time.Millisecond)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	projection, err = runtime.DNSInput(dns.ModuleName())
	if err != nil || len(projection.Sets) != 0 {
		t.Fatal("stopped producer retained live DHCP DNS records", projection, err)
	}
	update.Sequence++
	if err := observeMigration(ctx, runtime, update); err == nil {
		t.Fatal("expired wire grant was revived by a larger transport sequence")
	}
	active = makeSnapshot(dhcp, "laptop", "192.0.2.100", 4*time.Second)
	active.Sequence = 2
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, dhcp, 3, active)); err != nil {
		t.Fatal("fresh producer could not recover", err)
	}
	projection, err = runtime.DNSInput(dns.ModuleName())
	if err != nil || len(projection.Sets) != 1 || projection.Sets[0].Sequence != 2 {
		t.Fatal("fresh ownership did not restore the current lease", projection, err)
	}
	inactive := active
	inactive.Sequence, inactive.Active, inactive.Leases = 3, false, nil
	inactive.ObservedAt = time.Now().UTC()
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, dhcp, 4, inactive)); err != nil {
		t.Fatal(err)
	}
	projection, err = runtime.DNSInput(dns.ModuleName())
	if err != nil || len(projection.Sets) != 0 {
		t.Fatal("explicit producer withdrawal retained records", projection, err)
	}
	if !reflect.DeepEqual(initialNetwork, network.state()) {
		t.Fatal("DHCP lease/DNS ownership update changed static network intent")
	}
	// Removing the configured source also removes consumer authorization, even
	// though that producer remains a configured independently owned service.
	raw, _ := json.Marshal(doc)
	var changed config.Document
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	for _, resource := range changed.Resources {
		if resource.Service != nil && resource.Service.DNS != nil {
			resource.Service.DNS.Zones[0].Spec.DHCPDerived.Sources = nil
		}
	}
	if err := runtime.ApplyConfig(ctx, 2, changed); err != nil {
		t.Fatal(err)
	}
	remaining, err := runtime.DNSInput(dns.ModuleName())
	if err != nil || len(remaining.Sets) != 0 || len(remaining.Upstreams) != 0 {
		t.Fatal("removed source still contributed DNS input", remaining, err)
	}
}
