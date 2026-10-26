//go:build linux

package network

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func TestBootRouteKernelTimerIsIndependentOfUTC(t *testing.T) {
	c := routeClock{Clock: boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(2 * time.Hour), WallTime: time.Unix(1700000000, 0)}, monotonicNanos: int64(time.Hour)}
	original := lifetime.After(c.Clock, 100*365*24*3600)
	expected, err := newRouteTimer(original, c)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Nanos != uint64(c.monotonicNanos)+uint64(routeKernelGrant) {
		t.Fatal("long source lacks bounded kernel timer", expected)
	}
	for _, shift := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		stepped := c
		stepped.WallTime = stepped.WallTime.Add(shift)
		timer, err := newRouteTimer(original, stepped)
		if err != nil || timer != expected {
			t.Fatal("UTC changed physical kernel grant", timer, err)
		}
		want := ownedRoute{Route: Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: original}, KernelExpiry: expected}
		actual := RouteState{Route: Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: lifetime.Deadline{UTC: stepped.WallTime.Add(routeKernelGrant)}}, kernelExpiry: expected, Protocol: RouteProtocol, Type: 1}
		if !routeMatches(actual, want) {
			t.Fatal("UTC display caused false ownership drift")
		}
	}
	former := c
	former.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := newRouteTimer(original, former); err == nil {
		t.Fatal("former-boot route obtained fresh kernel lifetime")
	}
}

func TestWriterRenewsLongKernelTimerWithoutRenewingSourceAuthority(t *testing.T) {
	c, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, _ := engineFor(t, k)
	original := lifetime.After(c, 100*365*24*3600)
	d := Desired{Routes: []Route{{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: original}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	// A persisted long lease can have a kernel grant near renewal. Preserve
	// that actual readback while exercising the writer's renewal operation.
	clock, err := readRouteClock()
	if err != nil {
		t.Fatal(err)
	}
	e.state.Routes[0].KernelExpiry.Nanos = uint64(clock.monotonicNanos) + uint64(time.Hour)
	k.routes[0].kernelExpiry = e.state.Routes[0].KernelExpiry
	if _, err := e.expireAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.state.Routes) != 1 || len(k.routes) != 1 || e.state.Routes[0].ValidUntil != original || e.state.Routes[0].KernelExpiry.Nanos < uint64(clock.monotonicNanos)+uint64(6*24*time.Hour) {
		t.Fatal("kernel renewal changed source or failed to renew its own timer", e.state.Routes)
	}
}
