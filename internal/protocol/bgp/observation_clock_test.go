package bgp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

func TestPublicationPreservesEngineWalkObservationAge(t *testing.T) {
	cfg := moduleConfig(t)
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	clock.BoottimeNanoseconds -= int64(time.Second)
	clock.WallTime = clock.WallTime.Add(-time.Second)
	s := Snapshot{Active: true}
	bgpconfig.StampObservation(&s, clock)
	want := s
	var got Snapshot
	o := observation{cfg: cfg, epoch: strings.Repeat("a", 32), effects: EffectsFunc(func(_ context.Context, s Snapshot) error { got = s; return nil })}
	if err = o.publish(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got.ObservedNanos != want.ObservedNanos || got.ExpiresNanos != want.ExpiresNanos || !got.ObservedAt.Equal(want.ObservedAt) || !got.ValidUntil.Equal(want.ValidUntil) {
		t.Fatal("publication renewed a slow walk's evidence", got, want)
	}
	if err = o.publish(context.Background(), Snapshot{Active: true}); err == nil {
		t.Fatal("unstamped engine observation acquired a new publication lease")
	}
}
