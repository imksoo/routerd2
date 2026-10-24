package derived

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

var wall = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

const second = uint64(time.Second)
const ref = "DSLiteTunnel/ds-lite.status.phase"

func compile(t *testing.T, hysteresis string, initial bool) Program {
	t.Helper()
	p, err := Compile("derived", api.DerivedEventSpec{Topic: "routerd.virtual.dslite", Inputs: []api.ReadyWhenSpec{{Field: "${" + ref + "}", Equals: "Up"}}, Hysteresis: hysteresis, EmitInitial: initial})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func snapshot(t *testing.T, scope, origin, text string, known bool, now, expires uint64) Snapshot {
	t.Helper()
	s, err := NewSnapshot("sample", scope, now, wall.Add(time.Duration(now)), map[string]Value{ref: {Text: text, Known: known, Scope: scope, Origin: origin, ObservedNanos: now, ExpiresNanos: expires}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKnownAssertRetractAndInitialEmission(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_initial", true: "initial"}[initial], func(t *testing.T) {
			p := compile(t, "0s", initial)
			r, err := p.Advance(nil, snapshot(t, "boot/config", "worker-1", "Pending", true, second, 10*second), false)
			if err != nil || r.Status.Asserted == nil || *r.Status.Asserted {
				t.Fatal(r, err)
			}
			if len(r.Emissions) != map[bool]int{false: 0, true: 1}[initial] {
				t.Fatal("initial policy", r)
			}
			before := bytes.Clone(r.State)
			s := snapshot(t, "boot/config", "worker-1", "Up", true, 2*second, 10*second)
			up, err := p.Advance(r.State, s, false)
			if err != nil || len(up.Emissions) != 1 || up.Emissions[0].Event.Type != "routerd.virtual.dslite.asserted" || up.Status.Asserted == nil || !*up.Status.Asserted {
				t.Fatal(up, err)
			}
			retry, err := p.Advance(r.State, s, false)
			if err != nil || !reflect.DeepEqual(up, retry) || !bytes.Equal(r.State, before) {
				t.Fatal("retry changed output or caller state", err)
			}
			down, err := p.Advance(up.State, snapshot(t, "boot/config", "worker-1", "Pending", true, 3*second, 10*second), false)
			if err != nil || len(down.Emissions) != 1 || down.Emissions[0].Event.Type != "routerd.virtual.dslite.retracted" {
				t.Fatal(down, err)
			}
			again, err := p.Advance(down.State, snapshot(t, "boot/config", "worker-1", "Pending", true, 4*second, 10*second), false)
			if err != nil || len(again.Emissions) != 0 {
				t.Fatal("repeated stable state emitted again", again, err)
			}
		})
	}
}

func TestHysteresisFreshnessAuthorityAndFlipBack(t *testing.T) {
	p := compile(t, "1s", false)
	r, err := p.Advance(nil, snapshot(t, "boot/config", "worker-1", "Pending", true, second, 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker-1", "Up", true, 2*second, 5*second), false)
	if err != nil || r.Status.Phase != "Pending" || len(r.Emissions) != 0 {
		t.Fatal("did not hold assertion", r, err)
	}
	// A same-value fresh renewal keeps the original due time.
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker-1", "Up", true, 2500*uint64(time.Millisecond), 6*second), false)
	if err != nil || len(r.Emissions) != 0 {
		t.Fatal(r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker-1", "Up", true, 3*second, 6*second), false)
	if err != nil || len(r.Emissions) != 1 || !*r.Status.Asserted {
		t.Fatal("hold boundary failed", r, err)
	}
	asserted := bytes.Clone(r.State)
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker-1", "Pending", true, 4*second, 8*second), false)
	if err != nil || r.Status.PendingTransition != "Retracting" {
		t.Fatal(r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker-1", "Up", true, 4500*uint64(time.Millisecond), 8*second), false)
	if err != nil || len(r.Emissions) != 0 || r.Status.PendingTransition != "None" {
		t.Fatal("flip back did not cancel", r, err)
	}
	// Expired status is indeterminate, including after durable state restore.
	unknown, err := p.Advance(asserted, snapshot(t, "boot/config", "worker-1", "Up", true, 6*second, 6*second), false)
	if err != nil || unknown.Status.Asserted != nil || unknown.Status.Inputs != Indeterminate || len(unknown.Emissions) != 0 {
		t.Fatal("expired status revived assertion", unknown, err)
	}
	// A new producer cannot inherit a pending timer from its predecessor.
	base, err := p.Advance(nil, snapshot(t, "boot/config", "worker-1", "Pending", true, second, 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	base, err = p.Advance(base.State, snapshot(t, "boot/config", "worker-1", "Up", true, 2*second, 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	base, err = p.Advance(base.State, snapshot(t, "boot/config", "worker-2", "Up", true, 2900*uint64(time.Millisecond), 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	base, err = p.Advance(base.State, snapshot(t, "boot/config", "worker-2", "Up", true, 3*second, 10*second), false)
	if err != nil || len(base.Emissions) != 0 {
		t.Fatal("new producer inherited hold", base, err)
	}
}

func TestUnknownBootstrapAndLeaseGapRequireFreshHold(t *testing.T) {
	p := compile(t, "1s", false)
	r, err := p.Advance(nil, snapshot(t, "boot/config", "worker", "", false, second, 5*second), false)
	if err != nil || r.Status.Asserted != nil {
		t.Fatal(r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Up", true, 2*second, 2500*uint64(time.Millisecond)), false)
	if err != nil || len(r.Emissions) != 0 {
		t.Fatal(r, err)
	}
	// No evaluator call occurs in the lease gap; it must still reset the hold.
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Up", true, 3*second, 6*second), false)
	if err != nil || len(r.Emissions) != 0 {
		t.Fatal("expired interval renewed a hold", r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Up", true, 4*second, 6*second), false)
	if err != nil || len(r.Emissions) != 1 {
		t.Fatal(r, err)
	}
}

func TestSnapshotOwnershipAndOldScopeFailClosed(t *testing.T) {
	values := map[string]Value{ref: {Text: "Up", Known: true, Scope: "boot-1/config", Origin: "worker", ObservedNanos: second, ExpiresNanos: 5 * second}}
	s, err := NewSnapshot("one", "boot-1/config", second, wall, values)
	if err != nil {
		t.Fatal(err)
	}
	values[ref] = Value{Text: "Pending"}
	p := compile(t, "0s", true)
	r, err := p.Advance(nil, s, false)
	if err != nil || len(r.Emissions) != 1 || !*r.Status.Asserted {
		t.Fatal("snapshot retained caller map", r, err)
	}
	old, err := NewSnapshot("two", "boot-2/config", second, wall, map[string]Value{ref: {Text: "Up", Known: true, Scope: "boot-1/config", Origin: "worker", ObservedNanos: second, ExpiresNanos: 5 * second}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, old, false)
	if err != nil || r.Status.Asserted != nil || len(r.Emissions) != 0 {
		t.Fatal("old boot value became current permission", r, err)
	}
}

func TestGroupedInputAndKnownFalseDominateUnknown(t *testing.T) {
	spec := api.DerivedEventSpec{Topic: "routerd.derived", Inputs: []api.ReadyWhenSpec{{AnyOf: [][]api.ReadyWhenPredicateSpec{{{Field: "${" + ref + "}", Equals: "Up"}}, {{Field: "literal", Equals: "different"}}}}}}
	p, err := Compile("groups", spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Advance(nil, snapshot(t, "boot/config", "worker", "", false, second, 5*second), false)
	if err != nil || r.Status.Inputs != Indeterminate {
		t.Fatal(r, err)
	}
	spec.Inputs = append(spec.Inputs, api.ReadyWhenSpec{Field: "literal", Equals: "different"})
	p, err = Compile("groups", spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(json.RawMessage(nil), snapshot(t, "boot/config", "worker", "", false, second, 5*second), false)
	if err != nil || r.Status.Inputs != False || r.Status.Asserted == nil || *r.Status.Asserted {
		t.Fatal("known false did not dominate conjunction", r, err)
	}
}

func TestHysteresisUsesBoottimeAndBoundsCurrentStatusAtItsDeadline(t *testing.T) {
	p := compile(t, "1s", false)
	r, err := p.Advance(nil, snapshot(t, "boot/config", "worker", "Pending", true, second, 10*second), false)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Up", true, 2*second, 10*second), false)
	if err != nil || r.ValidUntilNanos != 3*second {
		t.Fatal("pending status can outlive its timer", r, err)
	}
	jump, err := NewSnapshot("wall-jump", "boot/config", 2500*uint64(time.Millisecond), wall.Add(24*time.Hour), map[string]Value{ref: {Text: "Up", Known: true, Scope: "boot/config", Origin: "worker", ObservedNanos: 2 * second, ExpiresNanos: 10 * second}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, jump, false)
	if err != nil || len(r.Emissions) != 0 {
		t.Fatal("wall jump completed monotonic hold", r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Up", true, 3*second, 10*second), false)
	if err != nil || len(r.Emissions) != 1 || !r.Emissions[0].Event.Time.Equal(wall.Add(3*time.Second)) {
		t.Fatal("timer changed its original event time", r, err)
	}
	r, err = p.Advance(r.State, snapshot(t, "boot/config", "worker", "Pending", true, 4*second, 10*second), false)
	if err != nil || r.ValidUntilNanos != 5*second || r.Status.Asserted == nil || !*r.Status.Asserted {
		t.Fatal("retracting true status has no bounded deadline", r, err)
	}
}

func TestZeroExpiryFutureAndMissingValuesStayIndeterminate(t *testing.T) {
	p := compile(t, "0s", true)
	for _, value := range []Value{
		{Text: "Up", Known: true, Scope: "boot/config", Origin: "worker", ObservedNanos: second, ExpiresNanos: 0},
		{Text: "Up", Known: true, Scope: "boot/config", Origin: "worker", ObservedNanos: 3 * second, ExpiresNanos: 5 * second},
		{Text: "Up", Known: false, Scope: "boot/config", Origin: "worker", ObservedNanos: second, ExpiresNanos: 5 * second},
	} {
		s, err := NewSnapshot("invalid-value", "boot/config", 2*second, wall, map[string]Value{ref: value})
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Advance(nil, s, false)
		if err != nil || len(r.Emissions) != 0 || r.Status.Asserted != nil || r.ValidUntilNanos != 0 {
			t.Fatal("invalid observation produced current assertion", r, err)
		}
	}
}

func TestPollingDoesNotRenewInputObservationTime(t *testing.T) {
	p := compile(t, "0s", false)
	values := map[string]Value{ref: {Text: "Up", Known: true, Scope: "boot/config", Origin: "worker", ObservedNanos: second, ExpiresNanos: 10 * second}}
	s, err := NewSnapshot("first", "boot/config", second, wall, values)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Advance(nil, s, false)
	if err != nil || r.ObservedNanos != second {
		t.Fatal(r, err)
	}
	s, err = NewSnapshot("poll", "boot/config", 5*second, wall, values)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, s, false)
	if err != nil || r.ObservedNanos != second {
		t.Fatal("polling renewed old Up evidence", r, err)
	}
}

func TestAnyTrueKeepsFreshnessAndExpiryFromTheSameWitness(t *testing.T) {
	p, err := Compile("witness", api.DerivedEventSpec{Topic: "routerd.witness", EmitWhen: "any_true", Inputs: []api.ReadyWhenSpec{{Field: "${Interface/a.status.phase}", Equals: "Up"}, {Field: "${Interface/b.status.phase}", Equals: "Up"}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSnapshot("one", "boot/config", 3*second, wall, map[string]Value{
		"Interface/a.status.phase": {Text: "Up", Known: true, Scope: "boot/config", Origin: "a", ObservedNanos: second, ExpiresNanos: 10 * second},
		"Interface/b.status.phase": {Text: "Up", Known: true, Scope: "boot/config", Origin: "b", ObservedNanos: 3 * second, ExpiresNanos: 4 * second},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Advance(nil, s, false)
	if err != nil || r.ObservedNanos != 3*second || r.ValidUntilNanos != 4*second {
		t.Fatal("freshness and expiry mixed different witnesses", r, err)
	}
}
