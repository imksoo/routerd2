package runtimeplan

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func bootLease4(clock boottime.Clock, lifetime time.Duration) dhcp4.Snapshot {
	now := uint64(clock.BoottimeNanoseconds)
	return dhcp4.Snapshot{BootID: clock.BootID, Epoch: clock.BootID, Sequence: now, ObservedNanos: now, Active: true, HardwareAddress: "02:00:00:00:00:01", LeaseClock: dhcp4.LeaseClock{BootID: clock.BootID, Epoch: clock.BootID, Sequence: now, AcquiredNanos: now, RenewNanos: now + uint64(lifetime/2), RebindNanos: now + uint64(lifetime*7/8), ExpiresNanos: now + uint64(lifetime)}, Snapshot: wire.Snapshot{Resource: "wan", Interface: "wan0", State: wire.StateBound, CurrentAddress: "192.0.2.10", PrefixLength: 24, DefaultGateway: "192.0.2.1", ServerID: "192.0.2.1", LeaseTimeSeconds: int64(lifetime / time.Second), AcquiredAt: clock.WallTime, RenewAt: clock.WallTime.Add(lifetime / 2), RebindAt: clock.WallTime.Add(lifetime * 7 / 8), ExpiresAt: clock.WallTime.Add(lifetime), UpdatedAt: clock.WallTime}}
}

func TestDHCPv4ProjectionUsesOriginalBootExpiryAcrossUTCChanges(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
	s := bootLease4(clock, 100*time.Second)
	raw, _ := json.Marshal(s)
	records := map[string]serviceconfig.Update{"service.wan": {Snapshot: raw}}
	for _, step := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		at := clock
		at.WallTime = at.WallTime.Add(step)
		p, err := projectWithFacts(wanDoc(), "/run/routerd", "/var/lib/routerd", records, at.WallTime, projectionFacts{bfdClock: at})
		if err != nil || len(p.network.Routes) != 1 || len(p.network.Interfaces[0].Addresses) != 1 {
			t.Fatal("UTC correction changed an unexpired lease", p.network, err)
		}
		o := p.network.Interfaces[0].AddressOptions["192.0.2.10/24"]
		if !o.ValidUntil.IsZero() || !o.PreferredUntil.IsZero() || o.BootLifetime.BootID != clock.BootID || o.BootLifetime.ValidNanos != s.LeaseClock.ExpiresNanos {
			t.Fatal("projection restamped lease from receipt/UTC", o)
		}
		at.BoottimeNanoseconds += int64(100 * time.Second)
		expired, err := projectWithFacts(wanDoc(), "/run/routerd", "/var/lib/routerd", records, at.WallTime, projectionFacts{bfdClock: at})
		if err != nil || len(expired.network.Routes) != 0 || len(expired.network.Interfaces[0].Addresses) != 0 {
			t.Fatal("expired original boot grant retained address/route", expired.network, err)
		}
	}
}

func TestDHCPv4OriginalBootGrantReplayMutationAndFinalFence(t *testing.T) {
	ctx := context.Background()
	r, n, _ := fixture(t, wanDoc())
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	s := bootLease4(clock, 2*time.Second)
	// Display UTC can differ from the receiver; it cannot extend the boot grant.
	s.AcquiredAt = s.AcquiredAt.Add(time.Hour)
	s.RenewAt, s.RebindAt, s.ExpiresAt, s.UpdatedAt = s.RenewAt.Add(time.Hour), s.RebindAt.Add(time.Hour), s.ExpiresAt.Add(time.Hour), s.UpdatedAt.Add(time.Hour)
	u := update(t, r, "service.wan", 1, s)
	if err := r.Accept(ctx, u); err != nil || len(n.applied.Routes) != 1 {
		t.Fatal(err, n.applied)
	}
	u.Sequence++
	if err := r.Accept(ctx, u); err != nil {
		t.Fatal("identical transport retry rejected", err)
	}
	changed := s
	changed.ObservedNanos++
	changed.CurrentAddress = "192.0.2.11"
	if err := r.Accept(ctx, update(t, r, "service.wan", 3, changed)); err == nil {
		t.Fatal("same lease grant changed its address without new ACK")
	}
	time.Sleep(2100 * time.Millisecond)
	u.Sequence = 4
	if err := r.Accept(ctx, u); err != nil {
		t.Fatal("expired duplicate should be harmlessly acknowledged", err)
	}
	if err := r.reconcileDynamic(ctx); err != nil || len(n.applied.Routes) != 0 || len(n.applied.Interfaces[0].Addresses) != 0 {
		t.Fatal("duplicate renewed expired address/route", err, n.applied)
	}
	fresh, _ := boottime.Read()
	final := bootLease4(fresh, time.Hour)
	final.Active, final.Stopped = false, true
	if err := r.Accept(ctx, update(t, r, "service.wan", 5, final)); err != nil {
		t.Fatal(err)
	}
	final.Active, final.Stopped = true, false
	final.ObservedNanos++
	if err := r.Accept(ctx, update(t, r, "service.wan", 6, final)); err == nil {
		t.Fatal("stopped worker regained authority")
	}
}

func TestDHCPv4FactUsesSourceAgeWithoutReceiptRenewal(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	source := conditionSource()
	if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
		t.Fatal(err)
	}
	clock, _ := boottime.Read()
	s := bootLease4(clock, time.Hour)
	if err := r.Accept(context.Background(), update(t, r, "service.wan", 1, s)); err != nil {
		t.Fatal(err)
	}
	g := r.guardGrants["service.wan"]
	if g.observedNanos != s.ObservedNanos || g.untilNanos != s.LeaseClock.ExpiresNanos || g.bootID != s.BootID {
		t.Fatal("fact age was minted at receipt", g)
	}
}
