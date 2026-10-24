package runtimeplan

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestDerivedInputKeepsOriginalAgeAndBreaksUnpolledFlapContinuity(t *testing.T) {
	ctx := context.Background()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	spec := api.DerivedEventSpec{Topic: "routerd.wan", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.phase}", Equals: "Healthy"}}, Hysteresis: "200ms"}
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1", Protocol: "tcp", Port: 80}}}, {ID: "derived", DerivedEvent: &spec}}}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	seq := uint64(0)
	var sample health.Snapshot
	publish := func(up bool) {
		t.Helper()
		seq++
		clock, err := bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		now := uint64(clock.BoottimeNanoseconds)
		phase, passed, failed := "Healthy", 1, 0
		if !up {
			phase, passed, failed = "Unhealthy", 0, 3
		}
		sample = health.Snapshot{Resource: "wan", Interface: "wan0", BootID: clock.BootID, ObservedNanos: now, ExpiresNanos: now + uint64(2*time.Minute), Active: up, State: old.State{Phase: phase, ConsecutivePassed: passed, ConsecutiveFailed: failed}}
		if err = r.Accept(ctx, update(t, r, "service.wan", seq, sample)); err != nil {
			t.Fatal(err)
		}
	}
	publish(false)
	_, s, err := r.DerivedInputs(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := derived.Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Advance(nil, s, false)
	if err != nil {
		t.Fatal(err)
	}
	publish(true)
	_, s, err = r.DerivedInputs(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	v, err = p.Advance(v.State, s, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.ObservedNanos != sample.ObservedNanos {
		t.Fatal("polling replaced original observation time", v.ObservedNanos, sample.ObservedNanos)
	}
	publish(false)
	publish(true) // no DerivedInputs or durable evaluation between callbacks
	time.Sleep(220 * time.Millisecond)
	_, s, err = r.DerivedInputs(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	v, err = p.Advance(v.State, s, false)
	if err != nil || len(v.Emissions) != 0 || v.Status.PendingTransition != "Asserting" {
		t.Fatal("unpolled flap inherited pending hold", v, err)
	}
	// Current status must not be rebuilt from the durable evaluator's old Up.
	r.mu.Lock()
	grant := r.guardGrants["service.wan"]
	grant.untilNanos = 1
	r.guardGrants["service.wan"] = grant
	r.mu.Unlock()
	_, s, err = r.DerivedInputs(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	v, err = p.Advance(json.RawMessage(v.State), s, false)
	if err != nil || v.Status.Asserted != nil || v.Status.Inputs != derived.Indeterminate {
		t.Fatal("expired observation remained current", v, err)
	}
}
