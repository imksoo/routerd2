//go:build linux

package bfd

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/derived"
)

func TestObservedTimestampContinuityBeforeMailboxCoalescing(t *testing.T) {
	for _, kind := range []string{"return_to_literal", "different_any_of_literal", "not_empty"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig()
			base := sampleClock(int64(10 * time.Second))
			w, err := newWorker(cfg, fakeDevice(1), base, func(uint32) uint32 { return 0 })
			if err != nil {
				t.Fatal(err)
			}
			first := base.WallTime.Add(time.Minute)
			last := first
			field := "${BFD/link.status.observedAt}"
			input := api.ReadyWhenSpec{Field: field, Equals: first.UTC().Format(time.RFC3339Nano)}
			if kind == "different_any_of_literal" {
				last = first.Add(time.Minute)
				input = api.ReadyWhenSpec{AnyOf: [][]api.ReadyWhenPredicateSpec{{{Field: field, Equals: first.UTC().Format(time.RFC3339Nano)}}, {{Field: field, Equals: last.UTC().Format(time.RFC3339Nano)}}}}
			} else if kind == "not_empty" {
				input = api.ReadyWhenSpec{Field: field, NotEmpty: true}
			}
			spec := api.DerivedEventSpec{Topic: "routerd.clock", Hysteresis: "1s", Inputs: []api.ReadyWhenSpec{input, {Field: "${HealthCheck/gate.status.phase}", Equals: "Up"}}}
			program, err := derived.Compile("clock", spec)
			if err != nil {
				t.Fatal(err)
			}
			sample := func(offset time.Duration, utc time.Time, gate string) derived.Snapshot {
				t.Helper()
				w.clock.BoottimeNanoseconds = base.BoottimeNanoseconds + int64(offset)
				w.clock.WallTime = utc
				status, known := CurrentStatus(cfg, w.snapshot(true, 0), "observedAt", w.clock)
				if !known {
					t.Fatal("current timestamp unavailable")
				}
				// Preserve all shared provider proof fields through the actual
				// wire shapes; this also executes against the pre-fix types.
				wire, _ := json.Marshal(status)
				var value derived.Value
				if err := json.Unmarshal(wire, &value); err != nil {
					t.Fatal(err)
				}
				value.Known, value.Scope, value.Origin = known, "scope", "same-worker"
				view, err := derived.NewSnapshot("sample", "scope", uint64(w.clock.BoottimeNanoseconds), utc, map[string]derived.Value{"BFD/link.status.observedAt": value, "HealthCheck/gate.status.phase": {Text: gate, Known: true, Scope: "scope", Origin: "gate-worker", ObservedNanos: uint64(w.clock.BoottimeNanoseconds), ExpiresNanos: uint64(w.clock.BoottimeNanoseconds) + uint64(5*time.Second)}})
				if err != nil {
					t.Fatal(err)
				}
				return view
			}
			down, err := program.Advance(nil, sample(0, base.WallTime, "Down"), false)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := program.Advance(down.State, sample(100*time.Millisecond, first, "Up"), false)
			if err != nil || pending.Status.PendingTransition != "Asserting" {
				t.Fatal(pending, err)
			}
			// The publisher creates an intervening nonmatching timestamp, but
			// its bounded mailbox replaces that notification before evaluation.
			_ = sample(200*time.Millisecond, first.Add(time.Second), "Up")
			reset, err := program.Advance(pending.State, sample(1200*time.Millisecond, last, "Up"), false)
			if kind == "not_empty" {
				if err != nil || len(reset.Emissions) != 1 || reset.Status.Asserted == nil || !*reset.Status.Asserted {
					t.Fatal("nonempty timestamp renewal cancelled a continuous predicate", reset, err)
				}
				return
			}
			if err != nil || len(reset.Emissions) != 0 || reset.Status.PendingTransition != "Asserting" {
				t.Fatal("unseen timestamp mismatch inherited the old hold", reset.Status, len(reset.Emissions), err)
			}
			stable, err := program.Advance(reset.State, sample(2300*time.Millisecond, last, "Up"), false)
			if err != nil || len(stable.Emissions) != 1 || stable.Status.Asserted == nil || !*stable.Status.Asserted {
				t.Fatal("actually unchanged timestamp could not complete its new hold", stable, err)
			}
		})
	}
}

func TestTimestampChangeSaturationAndUnchangedPublication(t *testing.T) {
	cfg := testConfig()
	w, err := newWorker(cfg, fakeDevice(1), sampleClock(int64(10*time.Second)), func(uint32) uint32 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	first := w.snapshot(true, 0)
	unchanged := w.snapshot(true, 0)
	if first.TimestampChanges != 1 || unchanged.TimestampChanges != first.TimestampChanges {
		t.Fatal("identical snapshot changed timestamp proof")
	}
	w.timestampChanges = math.MaxUint64 - 1
	w.clock.WallTime = w.clock.WallTime.Add(time.Second)
	w.clock.BoottimeNanoseconds += int64(time.Second)
	saturated := w.snapshot(true, 0)
	if saturated.TimestampChanges != math.MaxUint64 {
		t.Fatal("timestamp counter did not saturate")
	}
	if _, known := CurrentStatus(cfg, saturated, "observedAt", w.clock); known {
		t.Fatal("saturated timestamp proof certified equality continuity")
	}
	if got, known := CurrentStatus(cfg, saturated, "phase", w.clock); !known || got.Text != "Down" {
		t.Fatal("unrelated state field lost its independent proof", got, known)
	}
	w.clock.WallTime = w.clock.WallTime.Add(-time.Hour)
	w.clock.BoottimeNanoseconds += int64(time.Second)
	if next := w.snapshot(true, 0); next.TimestampChanges != math.MaxUint64 {
		t.Fatal("timestamp counter wrapped after a clock correction")
	}
}
