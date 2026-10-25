//go:build linux

package network

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func TestBootRouteAuthorityRenewsWithoutKernelChangesAndSurvivesWriterRestart(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, opts := engineFor(t, k)
	r := Route{Destination: "203.0.113.0/24", Interface: "wan0", BootID: clock.BootID, ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Second)}
	changed := r
	changed.Destination = "198.51.100.0/24"
	d := Desired{Routes: []Route{r, changed}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	writes := len(k.writes)
	for i := range d.Routes {
		d.Routes[i].ExpiresNanos += uint64(time.Second)
	}
	if _, err := e.Apply(context.Background(), 2, d); err != nil || len(k.writes) != writes {
		t.Fatal("authority renewal changed kernel routes", err, k.writes[writes:])
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.state.Routes[0].ExpiresNanos != d.Routes[0].ExpiresNanos {
		t.Fatal("writer restart lost the original renewed deadline")
	}
	// An external writer replaced one recorded route. Expiry must preserve it.
	k.routes[1].Protocol = 99
	time.Sleep(time.Until(clock.WallTime.Add(1100 * time.Millisecond)))
	if _, err := e.expireAuthority(context.Background()); err != nil || len(k.routes) != 2 {
		t.Fatal("the superseded deadline expired a renewed route", err)
	}
	time.Sleep(time.Until(clock.WallTime.Add(2100 * time.Millisecond)))
	if _, err := e.expireAuthority(context.Background()); err != nil || len(k.routes) != 1 || k.routes[0].Protocol != 99 || len(e.state.Routes) != 0 {
		t.Fatal("expiry retained owned state or deleted the foreign replacement", err, k.routes)
	}
	if _, err := e.Apply(context.Background(), 2, d); err != nil || len(k.routes) != 1 {
		t.Fatal("replay recreated expired authority", err, k.routes)
	}
	d.Routes[0].BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	d.Routes[0].ExpiresNanos += uint64(time.Hour)
	if _, err := e.Apply(context.Background(), 3, Desired{Routes: d.Routes[:1]}); err != nil || len(k.routes) != 1 {
		t.Fatal("foreign-boot replay recreated a route", err)
	}
}

func TestBootRouteDeadlineValidation(t *testing.T) {
	for _, r := range []Route{
		{BootID: "bad", ExpiresNanos: 1},
		{BootID: "11111111-2222-4333-8444-555555555555"},
		{ExpiresNanos: 1},
		{BootID: "11111111-2222-4333-8444-555555555555", ExpiresNanos: math.MaxUint64},
		{BootID: "11111111-2222-4333-8444-555555555555", ExpiresNanos: 1, ValidUntil: time.Now()},
	} {
		r.Destination, r.Interface = "203.0.113.0/24", "wan0"
		if err := Validate(Desired{Routes: []Route{r}}); err == nil {
			t.Fatal("invalid boot route deadline accepted", r)
		}
	}
}
