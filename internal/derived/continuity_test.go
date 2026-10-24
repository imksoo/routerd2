package derived

import (
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

func TestTrustedContinuityBridgesCoalescedRenewalsWithoutExtendingCurrentLease(t *testing.T) {
	p := compile(t, "1s", false)
	base, err := p.Advance(nil, snapshot(t, "scope", "producer", "Pending", true, second, 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	one, err := snapshot(t, "scope", "producer", "Up", true, 2*second, 2500*uint64(time.Millisecond)).WithContinuity("continuous-up", 2*second)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := p.Advance(base.State, one, false)
	if err != nil || len(pending.Emissions) != 0 {
		t.Fatal(pending, err)
	}
	// The trusted runtime saw all same-value renewals while evaluation was
	// coalesced. A different token/start cannot inherit that proof.
	later, err := snapshot(t, "scope", "producer", "Up", true, 4*second, 5*second).WithContinuity("continuous-up", 2*second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Advance(pending.State, later, false)
	if err != nil || len(r.Emissions) != 1 || r.Status.Asserted == nil || !*r.Status.Asserted {
		t.Fatal("continuous renewals lost the original hold", r, err)
	}
	expired, err := snapshot(t, "scope", "producer", "Up", true, 5*second, 5*second).WithContinuity("continuous-up", 2*second)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, expired, false)
	if err != nil || r.Status.Asserted != nil {
		t.Fatal("continuity token renewed an expired current lease", r, err)
	}
}

func TestContinuityTracksPredicateTruthRatherThanEveryScalarValue(t *testing.T) {
	spec := api.DerivedEventSpec{Topic: "routerd.scalar", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.message}", NotEmpty: true}}, Hysteresis: "1s"}
	p, err := Compile("scalar", spec)
	if err != nil {
		t.Fatal(err)
	}
	makeSnapshot := func(text string, now uint64) Snapshot {
		t.Helper()
		s, err := NewSnapshot("sample", "scope", now, wall, map[string]Value{"HealthCheck/wan.status.message": {Text: text, Known: true, Scope: "scope", Origin: "producer", ObservedNanos: now, ExpiresNanos: 10 * second}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := makeSnapshot("one", second), makeSnapshot("two", 2*second)
	if p.InputIdentity(a) != p.InputIdentity(b) || p.Proof(a).Value != True {
		t.Fatal("unchanged predicate truth changed authority")
	}
	x, err := p.Advance(nil, a, false)
	if err != nil {
		t.Fatal(err)
	}
	y, err := p.Advance(x.State, b, false)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := p.CheckpointChanged(x.State, y.State)
	if err != nil || changed {
		t.Fatal("stable poll forced a durable write", changed, err)
	}
}
