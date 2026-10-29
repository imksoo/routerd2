//go:build linux

package network

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func TestLinkAuthorityRenewalPersistsWithoutChangingLink(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	link := Link{Name: "ha", Kind: "macvlan", Parent: "lan0", HardwareAddr: "02:00:5e:00:01:42", IPv6AddrGenMode: "none", Authority: lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Hour)}}
	d := Desired{Links: []Link{link}, Routes: []Route{{Destination: "203.0.113.0/24", Interface: "ha"}}, Interfaces: []Interface{{Name: "ha", Addresses: []string{"192.0.2.42/32"}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	writes := len(k.writes)
	d.Links[0].Authority.Nanos += uint64(time.Hour)
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	for _, write := range k.writes[writes:] {
		if write != "address-update" {
			t.Fatal("lease renewal mutated link attributes", k.writes[writes:])
		}
	}
	if len(e.state.Addresses) != 1 || e.state.Addresses[0].Options.BootLifetime.ValidNanos != d.Links[0].Authority.Nanos {
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
	if len(e.state.Links) != 1 || e.state.Links[0].Definition.Authority != d.Links[0].Authority {
		t.Fatal("writer restart lost the renewed authority", e.state.Links)
	}

}

func TestLinkAddressBoundsPreserveTighterProtocolAndWallDeadlines(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(200 * time.Second), WallTime: time.Unix(1800000000, 0)}
	base := uint64(clock.BoottimeNanoseconds)
	d := Desired{Links: []Link{{Name: "ha", Kind: "macvlan", Parent: "wan0", Authority: lifetime.Deadline{BootID: clock.BootID, Nanos: base + uint64(10*time.Second)}}}, Interfaces: []Interface{
		{Name: "ha", Addresses: []string{"192.0.2.40/32", "192.0.2.41/32", "192.0.2.42/32"}, AddressOptions: map[string]AddressOptions{
			"192.0.2.41/32": {NoDAD: true, BootLifetime: BootLifetime{BootID: clock.BootID, PreferredNanos: base + uint64(3*time.Second), ValidNanos: base + uint64(5*time.Second)}},
			"192.0.2.42/32": {PreferredUntil: clock.WallTime.Add(2 * time.Second), ValidUntil: clock.WallTime.Add(15 * time.Second)},
		}},
		{Name: "wan0", Addresses: []string{"198.51.100.2/24"}},
	}}
	boundLinkAddresses(&d, clock)
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

func TestUTCLinkAuthorityFencesItsAddressAndRoute(t *testing.T) {
	k := fake()
	e, _ := engineFor(t, k)
	deadline := time.Now().Add(250 * time.Millisecond)
	d := Desired{
		Links:      []Link{{Name: "bounded", Kind: "dummy", Authority: lifetime.Deadline{UTC: deadline}}},
		Interfaces: []Interface{{Name: "bounded", Addresses: []string{"192.0.2.42/32"}}},
		Routes:     []Route{{Destination: "203.0.113.0/24", Interface: "bounded"}},
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if !e.state.Addresses[0].Options.ValidUntil.Equal(deadline) {
		t.Fatal("link's original UTC cutoff was changed", e.state.Addresses)
	}
	time.Sleep(time.Until(deadline) + 10*time.Millisecond)
	if _, err := e.expireAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.links["bounded"].Up || len(k.links["bounded"].Addresses) != 0 || len(k.routes) != 0 {
		t.Fatal("UTC authority retained a usable link, address or route")
	}
}
