//go:build linux

package network

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func TestBootAddressLifetimesIgnoreWallStepsAndRejectOtherBoots(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
	o := AddressOptions{BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: uint64(clock.BoottimeNanoseconds) + uint64(10*time.Second), ValidNanos: uint64(clock.BoottimeNanoseconds) + uint64(20*time.Second)}}
	for _, wall := range []time.Time{clock.WallTime, clock.WallTime.Add(24 * time.Hour), clock.WallTime.Add(-24 * time.Hour)} {
		p, v, err := addressLifetimesAt(o, wall, clock)
		if err != nil || p != 10 || v != 20 {
			t.Fatal("wall correction changed original lifetime", p, v, err)
		}
		live := AddressState{PreferredLifetime: 10, ValidLifetime: 20}
		if !addressOptionsMatchAt(live, o, wall, clock) {
			t.Fatal("clock correction lost legitimate ownership")
		}
		live.ValidLifetime = 200
		if addressOptionsMatchAt(live, o, wall, clock) {
			t.Fatal("foreign lifetime was adopted")
		}
	}
	clock.BoottimeNanoseconds += int64(15 * time.Second)
	if p, v, err := addressLifetimesAt(o, clock.WallTime, clock); err != nil || p != 0 || v != 5 {
		t.Fatal("BOOT progress did not deprecate address", p, v, err)
	}
	clock.BoottimeNanoseconds += int64(5 * time.Second)
	if _, v, err := addressLifetimesAt(o, clock.WallTime, clock); err != nil || v != 0 {
		t.Fatal("original BOOT deadline did not expire", v, err)
	}
	clock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := addressLifetimesAt(o, clock.WallTime, clock); err == nil {
		t.Fatal("former boot became current ownership")
	}
}

func TestBootAddressValidationDoesNotAdmitMixedOrInvalidClocks(t *testing.T) {
	good := BootLifetime{BootID: "11111111-2222-4333-8444-555555555555", PreferredNanos: 10, ValidNanos: 20}
	for _, bad := range []AddressOptions{
		{BootLifetime: good, PreferredUntil: time.Now()},
		{BootLifetime: good, ValidUntil: time.Now()},
		{BootLifetime: BootLifetime{BootID: good.BootID}},
		{BootLifetime: BootLifetime{BootID: "bad", PreferredNanos: 10, ValidNanos: 20}},
		{BootLifetime: BootLifetime{PreferredNanos: 10, ValidNanos: 20}},
		{BootLifetime: BootLifetime{BootID: good.BootID, PreferredNanos: 30, ValidNanos: 20}},
		{BootLifetime: BootLifetime{BootID: good.BootID, PreferredNanos: math.MaxInt64 + 1, ValidNanos: InfiniteNanos}},
	} {
		d := Desired{Interfaces: []Interface{{Name: "wan0", Addresses: []string{"192.0.2.9/24"}, AddressOptions: map[string]AddressOptions{"192.0.2.9/24": bad}}}}
		if err := Validate(d); err == nil {
			t.Fatal("invalid boot lifetime accepted", bad)
		}
	}
}

func TestBootAddressOwnershipSurvivesWriterRestartAndProtectsForeignLifetime(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, opts := engineFor(t, k)
	address := "192.0.2.9/24"
	o := AddressOptions{BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Hour), ValidNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Hour)}}
	d := Desired{Interfaces: []Interface{{Name: "wan0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: o}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if a := k.links["wan0"].Addresses[0]; a.ValidLifetime < 3598 || a.ValidLifetime > 3600 || a.ValidLifetime == math.MaxUint32 {
		t.Fatal("boot lifetime became infinite or used UTC", a)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	writes := len(k.writes)
	if _, err := e.Apply(context.Background(), 1, d); err != nil || len(k.writes) != writes {
		t.Fatal("restarted writer renewed an unchanged lifetime", err, k.writes[writes:])
	}
	l := k.links["wan0"]
	l.Addresses[0].ValidLifetime = 100000
	k.links["wan0"] = l
	if _, err := e.Apply(context.Background(), 2, Desired{}); err == nil || len(k.links["wan0"].Addresses) != 1 {
		t.Fatal("removed foreign modified address", err)
	}
}
