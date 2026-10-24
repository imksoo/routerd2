package derivedcontroller

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

var wall = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

const second = uint64(time.Second)

func declaration(t *testing.T) resourceprogram.DerivedEvent {
	t.Helper()
	spec := api.DerivedEventSpec{Topic: "routerd.derived", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.phase}", Equals: "Healthy"}}, EmitInitial: true, Hysteresis: "1s"}
	p, err := derived.Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	return resourceprogram.DerivedEvent{ID: "derived", Identity: "definition", Spec: spec, References: p.References(), Compiled: p}
}
func snapshot(t *testing.T, scope, text string, now, expires uint64, known bool) derived.Snapshot {
	t.Helper()
	s, err := derived.NewSnapshot("sample", scope, now, wall.Add(time.Duration(now)), map[string]derived.Value{"HealthCheck/wan.status.phase": {Text: text, Known: known, Scope: scope, Origin: "producer", ObservedNanos: now, ExpiresNanos: expires}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func outputCount(t *testing.T, s *events.Store, topic string) int {
	t.Helper()
	records, err := s.List(context.Background(), 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range records {
		if r.Event.Topic == topic {
			n++
		}
	}
	return n
}

func TestInitialOutputRetriesThroughActualSQLiteAndRestoredStateNeverGrantsCurrentUp(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "events")
	s, err := events.Open(events.Options{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s)
	rule := declaration(t)
	if err = c.Sync(ctx, 1, []resourceprogram.DerivedEvent{rule}); err != nil {
		t.Fatal(err)
	}
	injector, err := sql.Open("sqlite", filepath.Join(directory, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_derived_ack BEFORE UPDATE ON producer_outbox BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	up := snapshot(t, "boot/source", "Healthy", second, 10*second, true)
	if err = c.Step(ctx, map[string]derived.Snapshot{"derived": up}); err == nil {
		t.Fatal("expected real output ACK failure")
	}
	if outputCount(t, s, "routerd.derived.asserted") != 1 {
		t.Fatal("initial output not published before ACK failure")
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_derived_ack`); err != nil {
		t.Fatal(err)
	}
	c = New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.DerivedEvent{rule}); err != nil {
		t.Fatal(err)
	}
	missing := snapshot(t, "new-control/source", "Healthy", 2*second, 10*second, false)
	if err = c.Step(ctx, map[string]derived.Snapshot{"derived": missing}); err != nil {
		t.Fatal(err)
	}
	view, ok := c.View("derived", missing)
	if !ok || view.State.Asserted != nil {
		t.Fatal("durable asserted=true became current Up", view, ok)
	}
	if outputCount(t, s, "routerd.derived.asserted") != 1 {
		t.Fatal("ACK retry duplicated output")
	}
	if err = c.Sync(ctx, 2, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok = c.View("derived", up); ok {
		t.Fatal("removed resource remained active")
	}
}

func TestCurrentViewRejectsChangedInputAndExpiredEvaluatorLease(t *testing.T) {
	ctx := context.Background()
	s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.DerivedEvent{declaration(t)}); err != nil {
		t.Fatal(err)
	}
	up := snapshot(t, "boot/source", "Healthy", second, 20*second, true)
	if err = c.Step(ctx, map[string]derived.Snapshot{"derived": up}); err != nil {
		t.Fatal(err)
	}
	view, ok := c.View("derived", up)
	if !ok || view.State.Asserted == nil || !*view.State.Asserted {
		t.Fatal(view, ok)
	}
	down := snapshot(t, "boot/source", "Unhealthy", 2*second, 20*second, true)
	view, _ = c.View("derived", down)
	if view.State.Asserted != nil {
		t.Fatal("uncommitted transition exposed old Up", view)
	}
	late := snapshot(t, "boot/source", "Healthy", 10*second, 20*second, true)
	view, _ = c.View("derived", late)
	if view.State.Asserted != nil {
		t.Fatal("stopped evaluator renewed itself through reads", view)
	}
}
