package runtimeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func leaseServerDocument() config.Native {
	server := serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "192.0.2.1", Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.200", LeaseSeconds: 600}}
	dns := dnsconfig.RuntimeConfig{Spec: api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: 1053}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"lan"}}}}, Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lab.example", DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/leases"}}}}}}
	return config.Native{Services: []serviceconfig.Service{{Name: "leases", DHCPServer: &server}, {Name: "dns", DNS: &dns}}}
}

func TestDHCPServerDNSProjectionFencesSourcesAndExpiresReplays(t *testing.T) {
	doc := leaseServerDocument()
	r, _, _ := fixture(t, doc)
	initial, err := r.DNSLeases("service.dns")
	if err != nil || len(initial.Sets) != 0 || initial.ConfigDigest != r.bindings["service.dns"].digest {
		t.Fatalf("initial fenced empty projection: %+v %v", initial, err)
	}
	now := time.Now()
	snapshot := dhcpserverconfig.Snapshot{Resource: "leases", Interface: "lan0", ServerAddress: "192.0.2.1", Epoch: strings.Repeat("b", 32), Sequence: 1, Active: true, ObservedAt: now, ValidUntil: now.Add(5 * time.Second), Leases: []dhcpserverconfig.Lease{{MACAddress: "02:00:00:00:00:02", IPAddress: "192.0.2.123", Hostname: "laptop", Scope: "default", AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(600 * time.Second)}}}
	u := update(t, r, "service.leases", 1, snapshot)
	if err := r.Observe(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	projected, err := r.DNSLeases("service.dns")
	if err != nil || len(projected.Sets) != 1 || len(projected.Sets[0].Records) != 1 || projected.Sets[0].Records[0].Address != "192.0.2.123" {
		t.Fatalf("accepted lease not projected: %+v %v", projected, err)
	}
	bad := snapshot
	bad.Resource = "other"
	if err := r.Observe(context.Background(), update(t, r, "service.leases", 2, bad)); err == nil {
		t.Fatal("lease source identity spoof was accepted")
	}
	r.mu.Lock()
	r.leases["service.leases"] = time.Now().Add(-time.Second)
	r.mu.Unlock()
	// Simulate a replay transported with a higher envelope sequence. Its old
	// absolute expiry is still future, but its monotonic grant is exhausted.
	u.Sequence++
	if err := r.Observe(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	projected, err = r.DNSLeases("service.dns")
	if err != nil || len(projected.Sets) != 0 {
		t.Fatalf("replay extended DNS ownership: %+v %v", projected, err)
	}
	if _, err := r.DNSLeases("service.leases"); err == nil {
		t.Fatal("DHCP server obtained an unauthorized DNS consumer projection")
	}
}

func TestDNSLeaseSourceDependenciesAreValidatedBeforeApply(t *testing.T) {
	doc := leaseServerDocument()
	doc.Services[1].DNS.Zones[0].Spec.DHCPDerived.Sources = []string{"missing"}
	if err := config.Validate(config.FromNative(doc)); err == nil || !strings.Contains(err.Error(), "unresolved reference") {
		t.Fatalf("unresolved source was not rejected: %v", err)
	}
	doc = leaseServerDocument()
	doc.Services[0].DHCPServer.MaxLeases = 2048
	for _, zoneName := range []string{"extra-a", "extra-b"} {
		doc.Services[1].DNS.Zones = append(doc.Services[1].DNS.Zones, dnsconfig.RuntimeZone{Name: zoneName, Spec: api.DNSZoneSpec{Zone: zoneName + ".example", DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"leases"}}}})
	}
	if err := config.Validate(config.FromNative(doc)); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("unbounded combined lease projection was accepted: %v", err)
	}
}
