package events

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testEvent(id string) Event {
	return Event{ID: id, Topic: "routerd.resource.changed", Source: "fixture", ObservedAt: time.Unix(100, 0), Attributes: map[string]string{"resource": "wan"}}
}

func TestJournalReopenDedupAndNonExpiringEvent(t *testing.T) {
	ctx := context.Background()
	options := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("event-1")
	first, err := s.Append(ctx, event)
	if err != nil || first.Cursor == 0 {
		t.Fatal(first, err)
	}
	event.Attributes["resource"] = "caller-mutated"
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repeated, err := s.Append(ctx, testEvent("event-1"))
	if err != nil || repeated.Cursor != first.Cursor {
		t.Fatal("duplicate acquired another cursor", repeated, err)
	}
	if _, err := s.Append(ctx, event); !errors.Is(err, ErrConflict) {
		t.Fatal("ID reused with changed content", err)
	}
	rows, err := s.List(ctx, 0, 256)
	if err != nil || len(rows) != 1 || rows[0].Event.Attributes["resource"] != "wan" {
		t.Fatal(rows, err)
	}
	if rows[0].Event.IsExpired(time.Unix(1000000, 0)) {
		t.Fatal("zero expiry became a finite authority lease")
	}
}

func TestConsumerStartsAtTipAndRetriesAfterCursorWriteFailure(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, err := s.Append(ctx, testEvent("old"))
	if err != nil {
		t.Fatal(err)
	}
	start, err := s.Cursor(ctx, "consumer")
	if err != nil || start != old.Cursor {
		t.Fatal(start, err)
	}
	if _, err := s.Append(ctx, testEvent("next")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_cursor BEFORE UPDATE ON consumers BEGIN SELECT RAISE(ABORT, 'injected cursor write failure'); END`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	process := func(_ context.Context, r Record) error {
		if r.Event.ID != "next" {
			t.Fatal("preexisting history replayed", r)
		}
		calls++
		return nil
	}
	if err := s.Drain(ctx, "consumer", process); err == nil {
		t.Fatal("failed cursor save acknowledged")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(ctx, "consumer", process); err != nil || calls != 2 {
		t.Fatal("at-least-once retry lost", calls, err)
	}
	if err := s.Drain(ctx, "consumer", process); err != nil || calls != 2 {
		t.Fatal("acknowledged event replayed", calls, err)
	}
}

func TestOutboxAndCursorCommitTogetherAndCompletedWorkSurvivesCursorRollback(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cursor(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("work-event"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Key: "work-event/apply", Payload: json.RawMessage(`{"operation":"fixture"}`)}}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_cursor BEFORE UPDATE ON consumers BEGIN SELECT RAISE(ABORT, 'injected cursor write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(ctx, "consumer", 0, r.Cursor, jobs); err == nil {
		t.Fatal("partial outbox/cursor transaction accepted")
	}
	pending, err := s.Pending(ctx, "consumer", 256)
	if err != nil || len(pending) != 0 {
		t.Fatal("orphan work escaped rollback", pending, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(ctx, "consumer", 0, r.Cursor, jobs); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err = s.Pending(ctx, "consumer", 256)
	if err != nil || len(pending) != 1 || pending[0].Key != jobs[0].Key || pending[0].EventCursor != r.Cursor {
		t.Fatal(pending, err)
	}
	if err := s.Complete(ctx, "consumer", jobs[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE consumers SET cursor=0 WHERE name='consumer'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(ctx, "consumer", 0, r.Cursor, jobs); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending(ctx, "consumer", 256)
	if err != nil || len(pending) != 0 {
		t.Fatal("cursor rollback resurrected acknowledged work", pending, err)
	}
}

func TestJournalCapacityBackpressureDoesNotEraseUnconsumedHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"one", "two"} {
		if _, err := s.Append(ctx, testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(ctx, testEvent("three")); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity did not fail closed", err)
	}
	if _, err := s.Append(ctx, testEvent("one")); err != nil {
		t.Fatal("existing ID could not be acknowledged at capacity", err)
	}
	rows, err := s.List(ctx, 0, 256)
	if err != nil || len(rows) != 2 {
		t.Fatal("unconsumed journal was silently pruned", rows, err)
	}
}

func TestInvalidUTF8CannotChangeEventIdentityDuringEncoding(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	invalid := string([]byte{0xff})
	for _, field := range []string{"id", "topic", "source", "attribute"} {
		e := testEvent("invalid-" + field)
		switch field {
		case "id":
			e.ID = invalid
		case "topic":
			e.Topic = invalid
		case "source":
			e.Source = invalid
		case "attribute":
			e.Attributes["value"] = invalid
		}
		if _, err := s.Append(ctx, e); err == nil {
			t.Fatal("invalid UTF-8 accepted", field)
		}
	}
	rows, err := s.List(ctx, 0, 256)
	if err != nil || len(rows) != 0 {
		t.Fatal("invalid encoding changed journal", rows, err)
	}
}

func TestEventExpiryBoundaryRetainsFederationZeroExpiryContract(t *testing.T) {
	e := testEvent("finite")
	e.ExpiresAt = time.Unix(200, 0)
	if e.IsExpired(e.ExpiresAt) || !e.IsExpired(e.ExpiresAt.Add(time.Nanosecond)) {
		t.Fatal("event expiry boundary changed")
	}
	e.ExpiresAt = time.Time{}
	if e.IsExpired(time.Unix(1<<40, 0)) {
		t.Fatal("non-expiring event became a finite protocol lease")
	}
}

func TestAppendBatchIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prior := testEvent("prior")
	if _, err := s.Append(ctx, prior); err != nil {
		t.Fatal(err)
	}
	conflict := testEvent("prior")
	conflict.Attributes["resource"] = "changed"
	if err := s.AppendBatch(ctx, []Event{testEvent("new"), conflict}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 {
		t.Fatal("conflicting batch partially published", rows, err)
	}
	batch := []Event{prior, testEvent("new"), prior}
	for range 2 {
		if err := s.AppendBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	capacity, err := s.Capacity(ctx)
	if err != nil || capacity.Events.Rows != 2 || capacity.History.Rows != 2 {
		t.Fatal("replay consumed another budget", capacity, err)
	}
	if err := s.AppendBatch(ctx, []Event{testEvent("over-capacity")}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	rows, err = s.List(ctx, 0, 16)
	if err != nil || len(rows) != 2 {
		t.Fatal("full batch partially published", rows, err)
	}
}
