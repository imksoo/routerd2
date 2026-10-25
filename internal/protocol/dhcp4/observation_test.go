//go:build linux

package dhcp4

import (
	"context"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
)

func TestLeaseObservationPreservesACKTimebaseThroughSlowPublication(t *testing.T) {
	c, _, _, _ := fixture(t)
	clock := independentClock(t, c)
	bind(t, c)
	first := c.snapshot()
	if err := ValidateSnapshot(c.config, first); err != nil || !CurrentLease(first, *clock) || first.LeaseClock.ExpiresNanos-first.LeaseClock.AcquiredNanos != uint64(100*time.Second) {
		t.Fatal(first, err)
	}
	clock.BoottimeNanoseconds += int64(40 * time.Second)
	clock.WallTime = clock.WallTime.Add(-time.Hour)
	if err := c.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	renewing := c.snapshot()
	if renewing.LeaseClock != first.LeaseClock || !renewing.ExpiresAt.Equal(first.ExpiresAt) || renewing.ObservedNanos <= first.ObservedNanos {
		t.Fatal("T1 changed original grant", renewing, first)
	}
	c.persist = func(Snapshot) error {
		clock.BoottimeNanoseconds += int64(101 * time.Second)
		clock.WallTime = clock.WallTime.Add(24 * time.Hour)
		return nil
	}
	var sent Snapshot
	c.effects = EffectsFunc(func(_ context.Context, s Snapshot) error { sent = s; return nil })
	if err := c.handle(context.Background(), reply(c, wire.MessageACK)); err == nil {
		t.Fatal("expired original ACK was reported as successful")
	}
	if sent.LeaseClock.AcquiredNanos != renewing.ObservedNanos || sent.LeaseClock.ExpiresNanos != renewing.ObservedNanos+uint64(100*time.Second) || CurrentLease(sent, *clock) {
		t.Fatal("publication delay renewed boot authority", sent)
	}
}

func TestLeaseObservationValidationAndSavedHintCannotAuthorizeOldBoot(t *testing.T) {
	c, _, _, _ := fixture(t)
	clock := independentClock(t, c)
	bind(t, c)
	s := c.snapshot()
	for _, change := range []func(*Snapshot){
		func(s *Snapshot) { s.BootID = "bad" },
		func(s *Snapshot) { s.ObservedNanos = 0 },
		func(s *Snapshot) { s.LeaseClock.ExpiresNanos++ },
		func(s *Snapshot) { s.LeaseClock.RenewNanos = s.LeaseClock.RebindNanos },
		func(s *Snapshot) { s.LeaseClock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" },
		func(s *Snapshot) { s.ExpiresAt = s.ExpiresAt.Add(time.Hour) },
		func(s *Snapshot) { s.State = wire.StateDiscovering },
		func(s *Snapshot) { s.Stopped = true },
	} {
		bad := s
		change(&bad)
		if err := ValidateSnapshot(c.config, bad); err == nil {
			t.Fatal("invalid source observation accepted", bad)
		}
	}
	clock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if CurrentLease(s, *clock) {
		t.Fatal("former boot restored a current lease")
	}
	fresh, _, _, _ := fixture(t)
	newClock := independentClock(t, fresh)
	newClock.BootID = clock.BootID
	if err := fresh.start(context.Background(), &s); err != nil || fresh.snapshot().Active {
		t.Fatal("cached hint became active on another boot", err)
	}
	if err := fresh.handle(context.Background(), reply(fresh, wire.MessageACK)); err != nil || !CurrentLease(fresh.snapshot(), *newClock) {
		t.Fatal("fresh ACK did not establish the new boot", err)
	}
}
