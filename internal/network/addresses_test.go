//go:build linux

package network

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"
)

func TestRenewedDeadlineUpdatesKernelEvenWithinLifetimeRoundingTolerance(t *testing.T) {
	k := fake()
	e, _ := engineFor(t, k)
	address := "192.0.2.254/32"
	deadline := time.Now().Add(5500 * time.Millisecond)
	o := AddressOptions{PreferredUntil: deadline, ValidUntil: deadline}
	d := Desired{Interfaces: []Interface{{Name: "lan0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: o}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	k.writes = nil
	o.PreferredUntil, o.ValidUntil = deadline.Add(200*time.Millisecond), deadline.Add(200*time.Millisecond)
	d.Interfaces[0].AddressOptions[address] = o
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(k.writes, "address-update") {
		t.Fatal("renewal changed the journal without renewing the kernel timer", k.writes)
	}
	k.writes = nil
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(k.writes, "address-update") {
		t.Fatal("replay renewed the same original deadline")
	}
}

func TestPeerAddressOwnershipRestartAndDeletion(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	address := "192.0.0.2/32"
	d := Desired{Interfaces: []Interface{{Name: "wan0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: {Peer: "192.0.0.1/32"}}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	d.Interfaces[0].AddressOptions[address] = AddressOptions{Peer: "192.0.0.3/32"}
	if _, err := e.Apply(context.Background(), 2, d); err == nil {
		t.Fatal("changed an existing point-to-point peer in place")
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if len(k.links["wan0"].Addresses) != 0 {
		t.Fatal("peer address was not deleted by exact identity")
	}
}

func TestAddressRemovalRefusesPeerOrLifetimeDrift(t *testing.T) {
	for _, peer := range []bool{false, true} {
		k := fake()
		e, _ := engineFor(t, k)
		address := "192.0.0.2/32"
		d := Desired{Interfaces: []Interface{{Name: "wan0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: {Peer: "192.0.0.1/32"}}}}}
		if _, err := e.Apply(context.Background(), 1, d); err != nil {
			t.Fatal(err)
		}
		l := k.links["wan0"]
		if peer {
			l.Addresses[0].Peer = "192.0.0.3/32"
		} else {
			l.Addresses[0].ValidLifetime = 300
		}
		k.links["wan0"] = l
		if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
			t.Fatal("deleted externally changed address")
		}
		if len(k.links["wan0"].Addresses) != 1 {
			t.Fatal("externally changed address was removed")
		}
	}
}

func TestOwnedAddressLifetimesDeprecateRenewExpireAndRestoreStatic(t *testing.T) {
	k := fake()
	e, _ := engineFor(t, k)
	address := "2001:db8:1::1/64"
	d := Desired{Interfaces: []Interface{{Name: "lan0", Addresses: []string{address}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	o := AddressOptions{PreferredUntil: now.Add(time.Hour), ValidUntil: now.Add(2 * time.Hour)}
	d.Interfaces[0].AddressOptions = map[string]AddressOptions{address: o}
	if _, err := e.Apply(context.Background(), 2, d); err != nil {
		t.Fatal(err)
	}
	a := k.links["lan0"].Addresses[0]
	if a.PreferredLifetime < 3599 || a.ValidLifetime < 7199 {
		t.Fatalf("finite lifetimes not installed: %+v", a)
	}
	o.PreferredUntil = now.Add(-time.Second)
	d.Interfaces[0].AddressOptions[address] = o
	if _, err := e.Apply(context.Background(), 3, d); err != nil {
		t.Fatal(err)
	}
	if k.links["lan0"].Addresses[0].PreferredLifetime != 0 || k.links["lan0"].Addresses[0].ValidLifetime == 0 {
		t.Fatal("deprecated address should remain valid")
	}
	d.Interfaces[0].AddressOptions = nil
	if _, err := e.Apply(context.Background(), 4, d); err != nil {
		t.Fatal(err)
	}
	if k.links["lan0"].Addresses[0].PreferredLifetime != math.MaxUint32 || k.links["lan0"].Addresses[0].ValidLifetime != math.MaxUint32 {
		t.Fatal("static lifetimes not restored")
	}
	o.ValidUntil = now.Add(-time.Second)
	o.PreferredUntil = now.Add(-2 * time.Second)
	d.Interfaces[0].AddressOptions = map[string]AddressOptions{address: o}
	if _, err := e.Apply(context.Background(), 5, d); err != nil {
		t.Fatal(err)
	}
	s, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(k.links["lan0"].Addresses) != 0 || len(e.state.Addresses) != 0 || len(s.Desired.Interfaces[0].Addresses) != 0 {
		t.Fatal("expired address was not projected away")
	}
}

func TestAddressOptionsDoNotAdoptForeignAddressesOrIgnoreDrift(t *testing.T) {
	address := "2001:db8:1::1/64"
	k := fake()
	l := k.links["lan0"]
	l.Addresses = []AddressState{{Address: address, PreferredLifetime: math.MaxUint32, ValidLifetime: math.MaxUint32}}
	k.links["lan0"] = l
	e, _ := engineFor(t, k)
	d := Desired{Interfaces: []Interface{{Name: "lan0", Addresses: []string{address}, AddressOptions: map[string]AddressOptions{address: {NoDAD: true}}}}}
	if _, err := e.Apply(context.Background(), 1, d); err == nil {
		t.Fatal("adopted a foreign address")
	}
	l.Addresses = nil
	k.links["lan0"] = l
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if k.links["lan0"].Addresses[0].Flags&addressNoDADFlag == 0 {
		t.Fatal("explicit NoDAD was not set")
	}
	d.Interfaces[0].AddressOptions = nil
	if _, err := e.Apply(context.Background(), 2, d); err == nil {
		t.Fatal("silently changed DAD policy on an existing address")
	}
	d.Interfaces[0].AddressOptions = map[string]AddressOptions{address: {NoDAD: true}}
	l = k.links["lan0"]
	l.Addresses[0].ValidLifetime = 12
	l.Addresses[0].PreferredLifetime = 0
	k.links["lan0"] = l
	if _, err := e.Apply(context.Background(), 3, d); err == nil {
		t.Fatal("overwrote a foreign lifetime change")
	}
}

func TestInterruptedAddressLifetimeUpdateRecoversAfterRestart(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	address := "2001:db8:1::1/64"
	d := Desired{Interfaces: []Interface{{Name: "lan0", Addresses: []string{address}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	o := AddressOptions{PreferredUntil: time.Now().Add(-time.Second), ValidUntil: time.Now().Add(time.Hour)}
	e.state.Addresses[0].Attempted = &o
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := k.UpdateAddress("lan0", address, o); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if k.links["lan0"].Addresses[0].PreferredLifetime != math.MaxUint32 || e.state.Addresses[0].Attempted != nil {
		t.Fatal("interrupted lifetime change not rolled back")
	}
}

func TestAddressOptionValidation(t *testing.T) {
	now := time.Now()
	for _, d := range []Desired{
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.0.2/30"}, AddressOptions: map[string]AddressOptions{"192.0.0.2/30": {Peer: "192.0.0.1/30"}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.0.2/32"}, AddressOptions: map[string]AddressOptions{"192.0.0.2/32": {Peer: "2001:db8::1/128"}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.0.2/32"}, AddressOptions: map[string]AddressOptions{"192.0.0.2/32": {Peer: "192.0.0.2/32"}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}, AddressOptions: map[string]AddressOptions{"192.0.2.1/24": {NoDAD: true}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: map[string]AddressOptions{"2001:db8::2/64": {NoDAD: true}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: map[string]AddressOptions{"2001:db8::1/64": {ValidUntil: now}}}}},
		{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: map[string]AddressOptions{"2001:db8::1/64": {PreferredUntil: now.Add(time.Hour), ValidUntil: now}}}}},
	} {
		if err := Validate(d); err == nil {
			t.Errorf("accepted invalid address options: %+v", d)
		}
	}
}
