package derived

import (
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

func TestProviderContinuityFencesHoldWithoutChangingProducerAuthority(t *testing.T) {
	spec := api.DerivedEventSpec{Topic: "routerd.bfd", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/link.status.phase}", Equals: "Up"}}, Hysteresis: "1s"}
	p, err := Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	sample := func(text, continuity string, now uint64) Snapshot {
		t.Helper()
		s, err := NewSnapshot("sample", "scope", now, wall, map[string]Value{"BFD/link.status.phase": {Text: text, Known: true, Scope: "scope", Origin: "same-worker", Continuity: continuity, ObservedNanos: now, ExpiresNanos: 100 * second}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	down, err := p.Advance(nil, sample("Down", "a", second), false)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := p.Advance(down.State, sample("Up", "b", 2*second), false)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := p.Advance(pending.State, sample("Up", "c", 4*second), false)
	if err != nil || len(reset.Emissions) != 0 || reset.Status.PendingTransition != "Asserting" {
		t.Fatal("coalesced provider failure inherited old hold", reset, err)
	}
	up, err := p.Advance(reset.State, sample("Up", "c", 5*second), false)
	if err != nil || len(up.Emissions) != 1 || up.Status.Asserted == nil || !*up.Status.Asserted {
		t.Fatal(up, err)
	}
	retract, err := p.Advance(up.State, sample("Down", "d", 6*second), false)
	if err != nil || retract.Status.Asserted == nil || !*retract.Status.Asserted || retract.Status.PendingTransition != "Retracting" {
		t.Fatal("continuity was mistaken for replacement producer authority", retract, err)
	}
}

func TestBoundedStructuredStatusTextFitsSnapshotWithoutEnteringDurableState(t *testing.T) {
	p, err := Compile("derived", api.DerivedEventSpec{Topic: "routerd.peers", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/link.status.peers}", NotEmpty: true}}})
	if err != nil {
		t.Fatal(err)
	}
	value := Value{Text: strings.Repeat("x", 64<<10), Known: true, Scope: "scope", Origin: "worker", ObservedNanos: second, ExpiresNanos: 2 * second}
	s, err := NewSnapshot("sample", "scope", second, time.Unix(10, 0), map[string]Value{"BFD/link.status.peers": value})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Advance(nil, s, false)
	if err != nil || r.Status.Asserted == nil || !*r.Status.Asserted || len(r.State) > 4096 {
		t.Fatal("structured input was lost or copied into state", len(r.State), err)
	}
	value.Text += "x"
	if _, err = NewSnapshot("sample", "scope", second, wall, map[string]Value{"BFD/link.status.peers": value}); err == nil {
		t.Fatal("unbounded field accepted")
	}
}
