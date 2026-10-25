//go:build linux

package vrrp

import (
	"context"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func bootClock(nanos int64) boottime.Clock {
	return boottime.Clock{BootID: "50695aa4-d7d3-48c1-abab-14e150fae427", BoottimeNanoseconds: nanos, WallTime: time.Unix(1800000000, 0).UTC()}
}

func TestOriginalBootOwnershipIgnoresUTCAndExpiresAtExactTick(t *testing.T) {
	c := Normalize(config4())
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("a", 32))
	m.start(time.Now())
	m.enter(StateActive, time.Now())
	clock := bootClock(int64(time.Minute))
	if err := m.stampObservation(clock); err != nil {
		t.Fatal(err)
	}
	s := m.snapshot()
	if err := ValidateSnapshot(c, s); err != nil || s.BootID != clock.BootID || s.ObservedNanos != uint64(clock.BoottimeNanoseconds) || s.ExpiresNanos-s.ObservedNanos != uint64(ownershipLifetime(c)) {
		t.Fatal("publisher did not bind its original observation deadline", s, err)
	}
	for _, wall := range []time.Time{clock.WallTime.Add(-48 * time.Hour), clock.WallTime.Add(48 * time.Hour)} {
		clock.WallTime = wall
		s.ValidUntil = wall.Add(-time.Hour)
		clock.BoottimeNanoseconds = int64(s.ExpiresNanos - 1)
		if !CurrentOwnership(s, clock) {
			t.Fatal("UTC metadata changed current ownership")
		}
		clock.BoottimeNanoseconds++
		if CurrentOwnership(s, clock) {
			t.Fatal("ownership survived its original absolute BOOTTIME deadline")
		}
	}
	clock.BoottimeNanoseconds = int64(s.ObservedNanos)
	clock.BootID = "7bbe264e-57c0-4ee6-aa19-67c3a1d244fb"
	if CurrentOwnership(s, clock) {
		t.Fatal("old boot regained ownership")
	}
}

func TestObservationPublisherCannotResurrectExpiredOrRewoundAuthority(t *testing.T) {
	c := Normalize(config4())
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("b", 32))
	m.enter(StateActive, time.Now())
	clock := bootClock(int64(time.Minute))
	if err := m.stampObservation(clock); err != nil {
		t.Fatal(err)
	}
	first := m.snapshot()
	clock.BoottimeNanoseconds = int64(first.ExpiresNanos)
	if err := m.stampObservation(clock); err == nil {
		t.Fatal("a suspended/blocked active worker renewed an expired election without a transition")
	}
	clock.BoottimeNanoseconds = int64(first.ObservedNanos - 1)
	if err := m.stampObservation(clock); err == nil {
		t.Fatal("observation clock rewound")
	}
	clock.BoottimeNanoseconds = math.MaxInt64
	if err := m.stampObservation(clock); err == nil {
		t.Fatal("ownership clock overflow accepted")
	}
	m.enter(StateInitialize, time.Now())
	clock = bootClock(int64(first.ObservedNanos + 1))
	if err := m.stampObservation(clock); err != nil {
		t.Fatal(err)
	}
	stopped := m.snapshot()
	if stopped.ExpiresNanos != 0 || stopped.Active || CurrentOwnership(stopped, clock) {
		t.Fatal("shutdown retained finite ownership")
	}
}

func TestSnapshotRejectsMalformedBootAuthorityAndOverlongLease(t *testing.T) {
	c := Normalize(config4())
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("c", 32))
	m.enter(StateActive, time.Now())
	if err := m.stampObservation(bootClock(int64(time.Minute))); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.BootID = "" },
		func(s *Snapshot) { s.Epoch = "invalid" },
		func(s *Snapshot) { s.Transition = 0 },
		func(s *Snapshot) { s.ObservedNanos = 0 },
		func(s *Snapshot) { s.ExpiresNanos = s.ObservedNanos },
		func(s *Snapshot) { s.ExpiresNanos++ },
		func(s *Snapshot) { s.State = StateBackup },
	} {
		s := m.snapshot()
		mutate(&s)
		if err := ValidateSnapshot(c, s); err == nil {
			t.Fatal("malformed ownership accepted", s)
		}
	}
}

func TestBackupPublishesFreshFiniteNegativeObservationsWithoutTransition(t *testing.T) {
	c := Normalize(config4())
	device := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan Snapshot, 8)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("d", 32), device, EffectsFunc(func(_ context.Context, s Snapshot) (Decision, error) {
			updates <- s
			return Decision{Active: s.Active, Track: s.Track}, nil
		}), nil)
	}()
	var first, second Snapshot
	select {
	case first = <-updates:
	case <-time.After(time.Second):
		t.Fatal("initial backup observation missing")
	}
	select {
	case second = <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("backup state had no bounded observation heartbeat")
	}
	if first.Active || second.Active || first.State != StateBackup || second.State != StateBackup || first.Transition != second.Transition || second.ObservedNanos <= first.ObservedNanos || second.ExpiresNanos <= first.ExpiresNanos {
		t.Fatal("backup heartbeat changed role or reused old observation authority", first, second)
	}
	if err := ValidateSnapshot(c, second); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	final := <-updates
	if final.State != StateInitialize || final.ExpiresNanos != 0 {
		t.Fatal("backup shutdown retained authority")
	}
}

func TestExpiredActiveElectionCannotPublishFreshLeaseBeforeAdvertising(t *testing.T) {
	c := config4()
	c.AdvertIntervalCentiseconds = 1
	c = Normalize(c)
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("e", 32))
	m.enter(StateActive, time.Now())
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	clock.BoottimeNanoseconds -= int64(ownershipLifetime(c) + time.Millisecond)
	if err := m.stampObservation(clock); err != nil {
		t.Fatal(err)
	}
	device := newFakeTransport()
	published := false
	err = applyActions(context.Background(), m, actions{advertise: true}, device, EffectsFunc(func(context.Context, Snapshot) (Decision, error) { published = true; return Decision{}, nil }))
	if err == nil || published || len(device.sent) != 0 {
		t.Fatal("expired election renewed or advertised without a new transition", err, published, len(device.sent))
	}
}
