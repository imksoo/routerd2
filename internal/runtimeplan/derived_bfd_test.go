package runtimeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func derivedBFDSource(t *testing.T, hold string) (*Runtime, *observedManager, config.Document, derived.Program) {
	t.Helper()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	m := &observedManager{generation: 1}
	r.manager = m
	spec := api.DerivedEventSpec{Topic: "routerd.bfd.fixture", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/service/edge.status.phase}", Equals: "Up"}}, EmitInitial: true, Hysteresis: hold}
	source := config.FromNative(bfdDocument())
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "derived", DerivedEvent: &spec})
	if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	p, err := derived.Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	return r, m, source, p
}

func bfdDerivedSample(t *testing.T, r *Runtime, up bool, cycle uint64) bfd.Snapshot {
	t.Helper()
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	s := observedBFD(t, bfdDocument().Services[0].BFDConfig(), clock)
	s.Peers[0].Received = 3 + cycle*2
	if up {
		s.Peers[0].Transitions = 2 + cycle*2
		s.Peers[0].UpTransitions = 1 + cycle
		s.Peers[0].DownTransitions = cycle
	} else {
		s.Peers[0].State = bfd.StateDown
		s.Peers[0].RemoteState = bfd.StateDown
		s.Peers[0].DesiredMinTxMicroseconds = 1000000
		s.Peers[0].TransmitIntervalMicroseconds = 1000000
		s.Peers[0].DeadlineBootNanoseconds = 0
		s.Peers[0].ValidUntil = time.Time{}
		s.Peers[0].Transitions = 0
		s.Peers[0].UpTransitions = 0
		s.Peers[0].DownTransitions = 0
	}
	return s
}

func bfdDerivedInputs(t *testing.T, r *Runtime, source config.Document) derived.Snapshot {
	t.Helper()
	_, snapshots, err := r.DerivedInputs(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	return snapshots["derived"]
}

func TestDerivedBFDUsesOriginalPacketAgeAndExpiresWithoutInventingDown(t *testing.T) {
	r, m, source, p := derivedBFDSource(t, "")
	ctx := context.Background()
	before, err := p.Advance(nil, bfdDerivedInputs(t, r, source), false)
	if err != nil || before.Status.Asserted != nil {
		t.Fatal("declaration fabricated a live BFD source", before, err)
	}
	sample := bfdDerivedSample(t, r, true, 0)
	if err = r.Accept(ctx, update(t, r, "service.edge", 1, sample)); err != nil {
		t.Fatal(err)
	}
	up, err := p.Advance(before.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || up.Status.Asserted == nil || !*up.Status.Asserted || up.ObservedNanos != uint64(sample.Peers[0].LastReceivedBootNanoseconds) || up.ValidUntilNanos > uint64(sample.Peers[0].DeadlineBootNanoseconds) {
		t.Fatal(up, err)
	}
	refresh, err := p.Advance(up.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || refresh.ObservedNanos != up.ObservedNanos || refresh.ValidUntilNanos != up.ValidUntilNanos {
		t.Fatal("read renewed receive proof", refresh, err)
	}
	// Keep supervisor liveness fresh independently; the unrefreshed remote
	// receive deadline must still turn old raw UP into unknown.
	time.Sleep(time.Until(sample.Peers[0].LastReceivedAt.Add(950 * time.Millisecond)))
	m.generation = 1
	r.refreshGuardStatus(ctx)
	expired, err := p.Advance(refresh.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || expired.Status.Asserted != nil || len(expired.Emissions) != 0 {
		t.Fatal("expired raw UP was reported as a fresh Down", expired, err)
	}
}

func TestDerivedBFDCoalescedFailureBreaksUnpolledHoldAndWorkerIdentity(t *testing.T) {
	r, m, source, p := derivedBFDSource(t, "150ms")
	ctx := context.Background()
	if err := r.Accept(ctx, update(t, r, "service.edge", 1, bfdDerivedSample(t, r, false, 0))); err != nil {
		t.Fatal(err)
	}
	down, err := p.Advance(nil, bfdDerivedInputs(t, r, source), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Accept(ctx, update(t, r, "service.edge", 2, bfdDerivedSample(t, r, true, 0))); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Advance(down.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || pending.Status.PendingTransition != "Asserting" {
		t.Fatal(pending, err)
	}
	// The latest snapshot already returned to Up. No DerivedInputs callback
	// ran while the native BFD mailbox coalesced the intervening failure.
	if err = r.Accept(ctx, update(t, r, "service.edge", 3, bfdDerivedSample(t, r, true, 1))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(170 * time.Millisecond)
	reset, err := p.Advance(pending.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || len(reset.Emissions) != 0 || reset.Status.PendingTransition != "Asserting" {
		t.Fatal("coalesced Down inherited prior hold", reset, err)
	}
	time.Sleep(170 * time.Millisecond)
	asserted, err := p.Advance(reset.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || len(asserted.Emissions) != 1 || asserted.Status.Asserted == nil || !*asserted.Status.Asserted {
		t.Fatal(asserted, err)
	}
	next := update(t, r, "service.edge", 1, bfdDerivedSample(t, r, true, 0))
	next.Generation = 2
	next.ProducerEpoch = strings.Repeat("b", 32)
	if err = r.Accept(ctx, next); err != nil {
		t.Fatal(err)
	}
	unknown, err := p.Advance(asserted.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || unknown.Status.Asserted != nil {
		t.Fatal("unobserved generation became current", unknown, err)
	}
	m.generation = 2
	r.refreshGuardStatus(ctx)
	fresh, err := p.Advance(unknown.State, bfdDerivedInputs(t, r, source), false)
	if err != nil || fresh.Status.Asserted == nil || !*fresh.Status.Asserted {
		t.Fatal("fresh generation was not usable", fresh, err)
	}
}
