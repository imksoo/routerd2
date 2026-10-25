//go:build linux

package vrrp

import (
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
	if err := ValidateSnapshot(c, s); err != nil || s.BootID != clock.BootID || s.ObservedNanos != uint64(clock.BoottimeNanoseconds) || s.ExpiresNanos-s.ObservedNanos != uint64(OwnershipLifetime(c)) {
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
