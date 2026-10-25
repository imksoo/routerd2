package runtimeplan

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestBGPRecursiveResolutionECMPAndUnreachableAccounting(t *testing.T) {
	now := time.Now()
	service := serviceconfig.Service{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.10"}}}}
	d := config.Native{Services: []serviceconfig.Service{service}, Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.10/24"}}, {Name: "wan1", Addresses: []string{"198.51.100.10/24"}}}, Routes: []network.Route{{Destination: "10.8.0.0/16", Gateway: "192.0.2.1", Interface: "wan0"}}}}
	routes := []bgpconfig.Route{
		{Prefix: "203.0.113.0/24", NextHop: "192.0.2.2", Interface: "wan0"},
		{Prefix: "203.0.113.0/24", NextHop: "198.51.100.2", Interface: "wan1"},
		{Prefix: "10.9.0.0/16", NextHop: "10.8.0.9"},
		{Prefix: "10.11.0.0/16", NextHop: "10.12.0.9"},
		// This learned next hop recursively depends on itself and must not be installed.
		{Prefix: "10.12.0.0/16", NextHop: "10.12.0.9"},
	}
	snapshot := bgpconfig.Snapshot{Active: true, ValidUntil: now.Add(4 * time.Second), Routes: routes}
	clock, clockErr := bfd.ReadClock()
	if clockErr != nil {
		t.Fatal(clockErr)
	}
	bgpconfig.StampObservation(&snapshot, clock)
	raw, _ := json.Marshal(snapshot)
	p, e := projectWithFacts(d, "/run/routerd", "/var/lib/routerd", map[string]serviceconfig.Update{"service.edge": {Snapshot: raw}}, now, projectionFacts{bfdClock: clock})
	if e != nil {
		t.Fatal(e)
	}
	found := map[string]network.Route{}
	for _, route := range p.network.Routes {
		found[route.Destination] = route
	}
	if len(found["203.0.113.0/24"].NextHops) != 2 {
		t.Fatalf("ECMP paths were collapsed: %+v", p.network.Routes)
	}
	r := found["10.9.0.0/16"]
	if r.Gateway != "192.0.2.1" || r.Interface != "wan0" || r.Table != 254 || r.Metric != 200 {
		t.Fatalf("recursive gateway was not resolved: %+v", r)
	}
	if _, ok := found["10.12.0.0/16"]; ok {
		t.Fatal("recursive loop installed")
	}
	if len(p.routeIssues) != 2 || !strings.Contains(p.routeIssues[0].Reason, "loop") {
		t.Fatalf("unresolved path is not observable: %+v", p.routeIssues)
	}
}
func TestNextHopResolutionRespectsVRFTablesAndLinkLocalScope(t *testing.T) {
	index := fibIndex{}
	index.add(fibCandidate{prefix: netip.MustParsePrefix("192.0.2.0/24"), table: 100, hops: []network.NextHop{{Interface: "vrfwan", Weight: 1}}, connected: true})
	if _, e := index.resolve(netip.MustParseAddr("192.0.2.1"), 254, "", map[string]bool{}, 0); e == nil {
		t.Fatal("main table borrowed a VRF route")
	}
	hops, e := index.resolve(netip.MustParseAddr("192.0.2.1"), 100, "vrfwan", map[string]bool{}, 0)
	if e != nil || hops[0].Interface != "vrfwan" {
		t.Fatal(hops, e)
	}
	if _, e := index.resolve(netip.MustParseAddr("fe80::1"), 100, "", map[string]bool{}, 0); e == nil {
		t.Fatal("link-local next hop accepted without scope")
	}
	hops, e = index.resolve(netip.MustParseAddr("fe80::1"), 100, "vrfwan", map[string]bool{}, 0)
	if e != nil || hops[0].Gateway != "fe80::1" {
		t.Fatal(hops, e)
	}
}
