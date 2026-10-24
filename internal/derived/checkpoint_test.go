package derived

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
)

func TestDurableHoldTimerRetriesAtomicallyAndRequiresCurrentInput(t *testing.T) {
	ctx := context.Background()
	options := events.Options{Directory: filepath.Join(t.TempDir(), "journal")}
	s, err := events.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p := compile(t, "1s", false)
	cp, err := s.Checkpoint(ctx, "derived")
	if err != nil {
		t.Fatal(err)
	}
	// These input events anchor a consumer fixture; they are not current
	// protocol facts. Every evaluation below also supplies a finite snapshot.
	for i, text := range []string{"Pending", "Up"} {
		record, err := s.Append(ctx, events.Event{ID: text, Topic: "fixture.changed", Source: "fixture", ObservedAt: wall})
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Advance(cp.State, snapshot(t, "boot/config", "worker", text, true, uint64(i+1)*second, 10*second), false)
		if err != nil || len(r.Emissions) != 0 {
			t.Fatal(r, err)
		}
		if err = s.Transition(ctx, "derived", cp, record.Cursor, r.State, nil); err != nil {
			t.Fatal(err)
		}
		cp, err = s.Checkpoint(ctx, "derived")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = events.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.Checkpoint(ctx, "derived")
	if err != nil || !bytes.Equal(restored.State, cp.State) || restored.Version != cp.Version {
		t.Fatal("pending hold lost across reopen", restored, err)
	}
	due := snapshot(t, "boot/config", "worker", "Up", true, 3*second, 10*second)
	r, err := p.Advance(restored.State, due, false)
	if err != nil || len(r.Emissions) != 1 || r.Status.Asserted == nil || !*r.Status.Asserted {
		t.Fatal("restored timer did not complete with current input", r, err)
	}
	body, err := json.Marshal(r.Emissions[0])
	if err != nil {
		t.Fatal(err)
	}
	jobs := []events.Job{{Key: r.Emissions[0].ID, Payload: body}}
	injector, err := sql.Open("sqlite", filepath.Join(options.Directory, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_derived_timer BEFORE UPDATE ON consumers BEGIN SELECT RAISE(ABORT,'injected timer failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "derived", restored, restored.Cursor, r.State, jobs); err == nil {
		t.Fatal("expected actual checkpoint write failure")
	}
	current, err := s.Checkpoint(ctx, "derived")
	if err != nil || current.Version != restored.Version || !bytes.Equal(current.State, restored.State) {
		t.Fatal("failed timer persisted partial state", current, err)
	}
	if pending, err := s.Pending(ctx, "derived", 10); err != nil || len(pending) != 0 {
		t.Fatal("failed timer leaked emission", pending, err)
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_derived_timer`); err != nil {
		t.Fatal(err)
	}
	retry, err := p.Advance(current.State, due, false)
	if err != nil || !bytes.Equal(retry.State, r.State) || len(retry.Emissions) != 1 || retry.Emissions[0].ID != jobs[0].Key {
		t.Fatal("timer retry changed identity or state", retry, err)
	}
	if err = s.Transition(ctx, "derived", current, current.Cursor, retry.State, jobs); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "derived", current, current.Cursor, retry.State, jobs); !errors.Is(err, events.ErrConflict) {
		t.Fatal("stale timer claim was accepted", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = events.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	current, err = s.Checkpoint(ctx, "derived")
	if err != nil {
		t.Fatal(err)
	}
	work, err := s.Pending(ctx, "derived", 10)
	if err != nil || len(work) != 1 || work[0].Key != jobs[0].Key || !bytes.Equal(work[0].Payload, body) {
		t.Fatal("committed historical emission lost across reopen", work, err)
	}
	expired, err := p.Advance(current.State, snapshot(t, "boot/config", "worker", "Up", true, 10*second, 10*second), false)
	if err != nil || expired.Status.Asserted != nil || expired.Status.Inputs != Indeterminate || len(expired.Emissions) != 0 {
		t.Fatal("durable Up became current permission without a valid lease", expired, err)
	}
}
