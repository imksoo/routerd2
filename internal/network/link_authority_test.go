//go:build linux

package network

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func TestLinkAuthorityRenewsWithoutKernelChangesAndExpiresAfterWriterRestart(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	k := fake()
	e, opts := engineFor(t, k)
	link := Link{Name: "ha", Kind: "macvlan", Parent: "lan0", HardwareAddr: "02:00:5e:00:01:42", IPv6AddrGenMode: "none", BootID: clock.BootID, ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(500*time.Millisecond)}
	d := Desired{Links: []Link{link}, Interfaces: []Interface{{Name: "ha", Addresses: []string{"192.0.2.42/32"}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	writes := len(k.writes)
	d.Links[0].ExpiresNanos += uint64(600 * time.Millisecond)
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	for _, write := range k.writes[writes:] {
		if write != "address-update" {
			t.Fatal("lease renewal mutated link attributes", k.writes[writes:])
		}
	}
	if len(e.state.Addresses) != 1 || e.state.Addresses[0].Options.BootLifetime.ValidNanos != d.Links[0].ExpiresNanos {
		t.Fatal("link deadline did not bound its owned address", e.state.Addresses)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	// A foreign address on our link must survive activation expiry.
	live := k.links["ha"]
	live.Addresses = append(live.Addresses, AddressState{Address: "192.0.2.99/32"})
	k.links["ha"] = live
	time.Sleep(time.Until(clock.WallTime.Add(650 * time.Millisecond)))
	if _, err := e.expireAuthority(context.Background()); err != nil || !k.links["ha"].Up {
		t.Fatal("old deadline fenced a renewed owner", err)
	}
	time.Sleep(time.Until(clock.WallTime.Add(1150 * time.Millisecond)))
	if _, err := e.expireAuthority(context.Background()); err != nil || k.links["ha"].Up || len(k.links["ha"].Addresses) != 1 {
		t.Fatal("original deadline failed to fence link without deleting foreign address", err, k.links["ha"])
	}
	writes = len(k.writes)
	if _, err := e.Apply(context.Background(), 1, d); err == nil || len(k.writes) != writes {
		t.Fatal("expired input reactivated link", err, k.writes[writes:])
	}
	clock, err = boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	d.Links[0].ExpiresNanos = uint64(clock.BoottimeNanoseconds) + uint64(time.Second)
	if _, err := e.Apply(context.Background(), 1, d); err != nil || !k.links["ha"].Up {
		t.Fatal("fresh authority did not restore link", err)
	}
	// A replaced interface is never fenced on the old object's authority.
	live = k.links["ha"]
	live.Index++
	live.Alias = "foreign"
	k.links["ha"] = live
	time.Sleep(time.Until(clock.WallTime.Add(1050 * time.Millisecond)))
	if _, err := e.expireAuthority(context.Background()); err == nil || !k.links["ha"].Up {
		t.Fatal("retired identity modified a foreign replacement", err)
	}
}

func TestLinkAddressBoundsPreserveTighterProtocolAndWallDeadlines(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(200 * time.Second), WallTime: time.Unix(1800000000, 0)}
	base := uint64(clock.BoottimeNanoseconds)
	d := Desired{Links: []Link{{Name: "ha", Kind: "macvlan", Parent: "wan0", BootID: clock.BootID, ExpiresNanos: base + uint64(10*time.Second)}}, Interfaces: []Interface{
		{Name: "ha", Addresses: []string{"192.0.2.40/32", "192.0.2.41/32", "192.0.2.42/32"}, AddressOptions: map[string]AddressOptions{
			"192.0.2.41/32": {NoDAD: true, BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: base + uint64(3*time.Second), ValidNanos: base + uint64(5*time.Second)}},
			"192.0.2.42/32": {PreferredUntil: clock.WallTime.Add(2 * time.Second), ValidUntil: clock.WallTime.Add(15 * time.Second)},
		}},
		{Name: "wan0", Addresses: []string{"198.51.100.2/24"}},
	}}
	boundLinkAddresses(&d, clock, clock.WallTime)
	for _, want := range []struct {
		address          string
		preferred, valid time.Duration
	}{
		{"192.0.2.40/32", 10 * time.Second, 10 * time.Second},
		{"192.0.2.41/32", 3 * time.Second, 5 * time.Second},
		{"192.0.2.42/32", 2 * time.Second, 10 * time.Second},
	} {
		got := d.Interfaces[0].AddressOptions[want.address]
		if got.BootLifetime != (BootLifetime{BootID: clock.BootID, PreferredNanos: base + uint64(want.preferred), ValidNanos: base + uint64(want.valid)}) || !got.PreferredUntil.IsZero() || !got.ValidUntil.IsZero() {
			t.Fatal("wrong address authority intersection", want.address, got)
		}
	}
	if !d.Interfaces[0].AddressOptions["192.0.2.41/32"].NoDAD || len(d.Interfaces[1].AddressOptions) != 0 {
		t.Fatal("address bounds changed unrelated options or physical interface")
	}
}
