//go:build linux

package network

import (
	"context"
	"fmt"
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

func TestRouteTimerUpdateKeepsForwardingRouteAcrossWriterRestart(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint("kernel-ack-", acknowledged), func(t *testing.T) {
			clock, err := readRouteClock()
			if err != nil {
				t.Fatal(err)
			}
			k := fake()
			e, opts := engineFor(t, k)
			route := Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: lifetime.After(clock.Clock, 100*365*24*3600)}
			if _, err := e.Apply(context.Background(), 1, Desired{Routes: []Route{route}}); err != nil {
				t.Fatal(err)
			}
			// The writer persisted an update intent and stopped on either side
			// of its kernel ACK. Both readbacks still belong to this route.
			e.state.Routes[0].KernelExpiry.Nanos = uint64(clock.monotonicNanos) + uint64(time.Hour)
			k.routes[0].kernelExpiry = e.state.Routes[0].KernelExpiry
			timer, err := newRouteTimer(route.ValidUntil, clock)
			if err != nil {
				t.Fatal(err)
			}
			e.state.Routes[0].AttemptedExpiry = &timer
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if acknowledged {
				if err := k.UpdateRoute(ownedRoute{Route: route, KernelExpiry: timer}); err != nil {
					t.Fatal(err)
				}
			}
			_ = e.Close()
			restarted, err := newEngine(opts, k)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			k.writes = nil
			if _, err := restarted.expireAuthority(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(k.routes) != 1 || restarted.state.Routes[0].AttemptedExpiry != nil || !restarted.ownsRoute(k.routes[0]) {
				t.Fatal("writer lost the in-flight route update")
			}
			for _, op := range k.writes {
				if op == "route-delete" {
					t.Fatal("timer renewal withdrew the forwarding route")
				}
			}
		})
	}
}

func TestPendingRouteTimerRetainsCurrentKernelTimerBeforeAnotherIntent(t *testing.T) {
	clock, err := readRouteClock()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, opts := engineFor(t, k)
	route := Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: lifetime.After(clock.Clock, 100*365*24*3600)}
	if _, err := e.Apply(context.Background(), 1, Desired{Routes: []Route{route}}); err != nil {
		t.Fatal(err)
	}
	// The previous update reached the kernel, but the writer stopped before
	// recording its ACK. Another intent must retain that actual timer.
	currentTimer := e.state.Routes[0].KernelExpiry
	e.state.Routes[0].KernelExpiry.Nanos = uint64(clock.monotonicNanos) + uint64(time.Hour)
	e.state.Routes[0].AttemptedExpiry = &currentTimer
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	route.ValidUntil = lifetime.After(clock.Clock, 3*24*3600)
	k.fail = "route-update"
	if _, err := e.Apply(context.Background(), 2, Desired{Routes: []Route{route}}); err == nil {
		t.Fatal("kernel update failure was ignored")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !restarted.ownsRoute(k.routes[0]) {
		t.Fatal("new intent lost the prior acknowledged kernel timer")
	}
	k.fail, k.writes = "", nil
	if _, err := restarted.Apply(context.Background(), 2, Desired{Routes: []Route{route}}); err != nil {
		t.Fatal(err)
	}
	if restarted.state.Routes[0].AttemptedExpiry != nil || !restarted.ownsRoute(k.routes[0]) {
		t.Fatal("writer did not finish the pending refresh")
	}
	for _, operation := range k.writes {
		if operation == "route-delete" {
			t.Fatal("pending timer recovery withdrew the forwarding route")
		}
	}
}

func TestRouteRefreshBatchChecksAllOwnershipBeforeUpdates(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, _ := engineFor(t, k)
	d := Desired{Routes: []Route{
		{Destination: "2001:db8:1::/64", Interface: "wan0", ValidUntil: lifetime.After(clock, 60)},
		{Destination: "2001:db8:2::/64", Interface: "wan0", ValidUntil: lifetime.After(clock, 60)},
	}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	k.routes[1].Protocol = 99
	k.writes = nil
	for i := range d.Routes {
		d.Routes[i].ValidUntil = lifetime.After(clock, 120)
	}
	if _, err := e.Apply(context.Background(), 2, d); err == nil {
		t.Fatal("changed foreign route was adopted")
	}
	for _, operation := range k.writes {
		if operation == "route-update" {
			t.Fatal("batch updated a route before discovering the ownership conflict")
		}
	}
}
