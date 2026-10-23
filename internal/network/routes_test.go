//go:build linux

package network

import (
	"context"
	"net"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func ecmpExample() Desired {
	return Desired{Routes: []Route{{Destination: "203.0.113.0/24", Metric: 42, NextHops: []NextHop{
		{Gateway: "198.51.100.1", Interface: "wan0", Weight: 256},
		{Gateway: "192.0.2.1", Interface: "lan0", Weight: 3},
	}}}}
}

func TestECMPJournalRestartReorderPartialUpdateAndRollback(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	d := ecmpExample()
	before := cloneDesired(d)
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, before) {
		t.Fatal("normalization mutated caller's next-hop slice")
	}
	if !e.ownsRoute(k.routes[0]) || len(k.routes[0].NextHops) != 2 {
		t.Fatal("ECMP route ownership was not recorded")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	k.writes = nil
	k.routes[0].NextHops[0], k.routes[0].NextHops[1] = k.routes[0].NextHops[1], k.routes[0].NextHops[0]
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if len(k.writes) != 0 {
		t.Fatalf("ECMP reorder triggered writes: %v", k.writes)
	}
	changed := cloneDesired(d)
	changed.Routes[0].NextHops[0].Weight = 1
	k.fail = "route-add"
	if _, err := e.Apply(context.Background(), 2, changed); err == nil {
		t.Fatal("injected ECMP replacement failure ignored")
	}
	if len(k.routes) != 0 || len(e.state.Routes) != 0 {
		t.Fatal("removed route retained as installed after partial replacement")
	}
	k.fail = ""
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if len(k.routes) != 1 || !sameRoute(k.routes[0].Route, d.Routes[0]) {
		t.Fatal("committed ECMP weights not restored")
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if len(k.routes) != 0 {
		t.Fatal("owned ECMP route remains")
	}
}

func TestECMPDoesNotAdoptMatchingProtocolOrOverwriteWeightDrift(t *testing.T) {
	for _, mode := range []string{"no-journal", "weight-drift", "advanced-hop"} {
		t.Run(mode, func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			d := ecmpExample()
			if mode == "no-journal" {
				if err := k.AddRoute(d.Routes[0]); err != nil {
					t.Fatal(err)
				}
			} else if _, err := e.Apply(context.Background(), 1, d); err != nil {
				t.Fatal(err)
			}
			if mode == "weight-drift" {
				k.routes[0].NextHops[0].Weight++
			}
			if mode == "advanced-hop" {
				k.routes[0].Advanced = true
			}
			k.writes = nil
			if _, err := e.Apply(context.Background(), 2, d); err == nil {
				t.Fatal("unowned or changed ECMP route was accepted")
			}
			if len(k.writes) != 0 || e.ownsRoute(k.routes[0]) {
				t.Fatal("foreign ECMP route was adopted or modified")
			}
		})
	}
}

func TestECMPValidationAndSingleHopNormalization(t *testing.T) {
	for _, edit := range []func(*Route){
		func(r *Route) { r.Interface = "wan0" },
		func(r *Route) { r.Gateway = "198.51.100.1" },
		func(r *Route) { r.NextHops[0].Weight = 0 },
		func(r *Route) { r.NextHops[0].Weight = 257 },
		func(r *Route) { r.NextHops[0].Interface = "" },
		func(r *Route) { r.NextHops[0].Gateway = "2001:db8::1" },
		func(r *Route) { r.NextHops[0].Gateway = "224.0.0.1" },
		func(r *Route) { r.NextHops[0] = r.NextHops[1] },
	} {
		d := ecmpExample()
		edit(&d.Routes[0])
		if err := Validate(d); err == nil {
			t.Errorf("accepted invalid ECMP route: %+v", d.Routes[0])
		}
	}
	d := ecmpExample()
	d.Routes[0].NextHops = d.Routes[0].NextHops[:1]
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	n := normalizeRoute(d.Routes[0])
	if len(n.NextHops) != 0 || n.Gateway != "198.51.100.1" || n.Interface != "wan0" {
		t.Fatalf("single next hop was not canonicalized: %+v", n)
	}
}

func TestNativeECMPObservationPreservesWeightsAndUnknownPolicy(t *testing.T) {
	_, dst, _ := net.ParseCIDR("2001:db8:3::/64")
	r := netlink.Route{Dst: dst, Table: 254, Priority: 1024, Family: netlink.FAMILY_V6, Type: unix.RTN_UNICAST, Protocol: RouteProtocol, MultiPath: []*netlink.NexthopInfo{
		{LinkIndex: 2, Gw: net.ParseIP("2001:db8:2::1"), Hops: 255, Flags: unix.RTNH_F_LINKDOWN},
		{LinkIndex: 1, Gw: net.ParseIP("2001:db8:1::1"), Hops: 2},
	}}
	want := Route{Destination: "2001:db8:3::/64", NextHops: []NextHop{{Gateway: "2001:db8:1::1", Interface: "lan0", Weight: 3}, {Gateway: "2001:db8:2::1", Interface: "wan0", Weight: 256}}}
	names := map[int]string{1: "lan0", 2: "wan0"}
	s := nativeRouteState(r, names)
	if !routeMatches(s, want) || s.Advanced {
		t.Fatalf("native weighted route not recognized: %+v", s)
	}
	r.MultiPath[0].Flags |= unix.RTNH_F_ONLINK
	if routeMatches(nativeRouteState(r, names), want) {
		t.Fatal("foreign ONLINK next-hop flag ignored")
	}
}

func TestForeignECMPReferencePreventsOwnedLinkDeletion(t *testing.T) {
	k := fake()
	e, _ := engineFor(t, k)
	d := Desired{Links: []Link{{Name: "br0", Kind: "bridge"}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	foreign := Route{Destination: "198.18.0.0/24", NextHops: []NextHop{{Interface: "br0", Weight: 1}, {Interface: "lan0", Weight: 1}}}
	if err := k.AddRoute(foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 2, Desired{}); err == nil {
		t.Fatal("deleted a link referenced by a foreign ECMP route")
	}
	if _, ok := k.links["br0"]; !ok || len(k.routes) != 1 {
		t.Fatal("foreign route reference or owned link was removed")
	}
}
