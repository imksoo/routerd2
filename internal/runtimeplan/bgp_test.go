package runtimeplan

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
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
	p, e := projectWithFacts(d, protocolsFor(t, d, "/run/routerd", "/var/lib/routerd"), map[string]serviceconfig.Update{"service.edge": {Snapshot: raw}}, now, projectionFacts{bfdClock: clock})
	if e != nil {
		t.Fatal(e)
	}
	found := map[string]network.Route{}
	for _, route := range p.network.Routes {
		found[route.Destination] = route
		if route.Destination != "10.8.0.0/16" && (route.BootID != snapshot.BootID || route.ExpiresNanos != snapshot.ExpiresNanos) {
			t.Fatal("learned route did not carry its original observation deadline", route)
		}
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
	index.add(fibCandidate{prefix: netip.MustParsePrefix("192.0.2.0/24"), table: 100, hops: []network.NextHop{{Interface: "vrfwan", Weight: 1}}})
	if _, _, e := index.resolve(netip.MustParseAddr("192.0.2.1"), 254, "", map[string]bool{}, 0); e == nil {
		t.Fatal("main table borrowed a VRF route")
	}
	hops, _, e := index.resolve(netip.MustParseAddr("192.0.2.1"), 100, "vrfwan", map[string]bool{}, 0)
	if e != nil || hops[0].Interface != "vrfwan" {
		t.Fatal(hops, e)
	}
	if _, _, e := index.resolve(netip.MustParseAddr("fe80::1"), 100, "", map[string]bool{}, 0); e == nil {
		t.Fatal("link-local next hop accepted without scope")
	}
	hops, _, e = index.resolve(netip.MustParseAddr("fe80::1"), 100, "vrfwan", map[string]bool{}, 0)
	if e != nil || hops[0].Gateway != "fe80::1" {
		t.Fatal(hops, e)
	}
}

func TestLearnedRouteRetainsRecursiveAddressRouteAndLinkDeadlines(t *testing.T) {
	clock := bfd.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Unix(1800000000, 0).UTC()}
	base := uint64(clock.BoottimeNanoseconds)
	service := serviceconfig.Service{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.10"}}}}
	for _, prerequisite := range []string{"address", "route", "utc-route", "link", "link-local"} {
		t.Run(prerequisite, func(t *testing.T) {
			d := config.Native{Services: []serviceconfig.Service{service}, Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.10/24"}}}, Routes: []network.Route{{Destination: "10.8.0.0/16", Gateway: "192.0.2.1", Interface: "wan0"}}}}
			hop, family := "10.8.0.9", "203.0.113.0/24"
			switch prerequisite {
			case "address":
				d.Network.Interfaces[0].AddressOptions = map[string]network.AddressOptions{"192.0.2.10/24": {BootLifetime: network.BootLifetime{BootID: clock.BootID, PreferredNanos: base + uint64(time.Second), ValidNanos: base + uint64(2*time.Second)}}}
			case "route":
				d.Network.Routes[0].BootID = clock.BootID
				d.Network.Routes[0].ExpiresNanos = base + uint64(2*time.Second)
			case "utc-route":
				d.Network.Interfaces[0].Addresses = []string{"2001:db8:1::10/64"}
				d.Network.Routes[0] = network.Route{Destination: "2001:db8:8::/64", Gateway: "2001:db8:1::1", Interface: "wan0"}
				d.Network.Routes[0].ValidUntil = clock.WallTime.Add(2 * time.Second)
				hop, family = "2001:db8:8::9", "2001:db8:77::/64"
			case "link", "link-local":
				d.Network.Links = []network.Link{{Name: "wan0", Kind: "macvlan", Parent: "uplink", Authority: lifetime.Deadline{BootID: clock.BootID, Nanos: base + uint64(2*time.Second)}}}
				if prerequisite == "link-local" {
					hop, family = "fe80::1", "2001:db8:77::/64"
				}
			}
			snapshot := bgpconfig.Snapshot{Active: true, Routes: []bgpconfig.Route{{Prefix: family, NextHop: hop, Interface: "wan0"}}}
			bgpconfig.StampObservation(&snapshot, clock)
			if snapshot.ExpiresNanos <= base+uint64(2*time.Second) {
				t.Fatal("fixture BGP lease does not outlive its prerequisite")
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			p := projection{network: d.Network}
			protocols := protocolsFor(t, d, "/run/routerd", "/var/lib/routerd")
			if err := projectBGP(&p, d, protocols.bgp, map[string]serviceconfig.Update{"service.edge": {Snapshot: raw}}, clock.WallTime, clock); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, route := range p.network.Routes {
				if route.Destination == family {
					if route.BootID != clock.BootID || route.ExpiresNanos != base+uint64(2*time.Second) {
						t.Fatal("recursive resolution discarded prerequisite authority", route)
					}
					found = true
				}
			}
			if !found {
				t.Fatal("reachable learned route was not projected", p.routeIssues)
			}
			expired := clock
			expired.BoottimeNanoseconds += int64(2100 * time.Millisecond)
			expired.WallTime = expired.WallTime.Add(2100 * time.Millisecond)
			p = projection{network: d.Network}
			if err := projectBGP(&p, d, protocols.bgp, map[string]serviceconfig.Update{"service.edge": {Snapshot: raw}}, expired.WallTime, expired); err != nil {
				t.Fatal(err)
			}
			for _, route := range p.network.Routes {
				if route.Destination == family {
					t.Fatal("expired prerequisite still authorized a learned route", route)
				}
			}
			if len(p.routeIssues) != 1 {
				t.Fatal("expired next hop was not reported", p.routeIssues)
			}
		})
	}
}
