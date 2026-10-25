package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestBFDTimestampEqualityRuntimeAndProducerCounterFence(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	literal := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	spec := api.DerivedEventSpec{Topic: "routerd.clock", Hysteresis: "150ms", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/service/edge.status.observedAt}", Equals: literal.Format(time.RFC3339Nano)}}}
	source := config.FromNative(bfdDocument())
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "derived", DerivedEvent: &spec})
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	p, err := derived.Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := bfdDerivedSample(t, r, false, 0)
	publish := func(sequence, changes uint64, wall time.Time) error {
		t.Helper()
		clock, err := bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.ObservedNanos = uint64(clock.BoottimeNanoseconds)
		snapshot.ExpiresNanos = snapshot.ObservedNanos + uint64(bfd.StatusLifetime)
		snapshot.TimestampChanges = changes
		snapshot.UpdatedAt = wall
		return r.Observe(ctx, update(t, r, "service.edge", sequence, snapshot))
	}
	if err = publish(1, 1, literal.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	down, err := p.Advance(nil, bfdDerivedInputs(t, r), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = publish(2, 2, literal); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Advance(down.State, bfdDerivedInputs(t, r), false)
	if err != nil || pending.Status.PendingTransition != "Asserting" {
		t.Fatal(pending, err)
	}
	time.Sleep(170 * time.Millisecond)
	if err = publish(3, 4, literal); err != nil {
		t.Fatal(err)
	}
	reset, err := p.Advance(pending.State, bfdDerivedInputs(t, r), false)
	if err != nil || len(reset.Emissions) != 0 || reset.Status.PendingTransition != "Asserting" {
		t.Fatal("runtime discarded the coalesced timestamp continuity", reset.Status, err)
	}
	if err = publish(4, 3, literal); err == nil {
		t.Fatal("timestamp counter rewind accepted")
	}
	if err = publish(4, 4, literal.Add(time.Second)); err == nil {
		t.Fatal("changed timestamp without counter advance accepted")
	}
	time.Sleep(170 * time.Millisecond)
	stable, err := p.Advance(reset.State, bfdDerivedInputs(t, r), false)
	if err != nil || len(stable.Emissions) != 1 {
		t.Fatal("rejected producer evidence changed the accepted source", stable, err)
	}
}
