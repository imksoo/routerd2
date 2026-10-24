package events

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

func TestCapacityIncludesCompletedConsumerAndProducerWorkWithoutRegistering(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 2, MaxJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	empty, err := s.Capacity(ctx)
	if err != nil || empty.Events.Rows != 0 || empty.Events.RowLimit != 2 || empty.Registrations.Rows != 0 || empty.TipCursor != 0 {
		t.Fatal(empty, err)
	}
	if _, found, err := s.LookupProducer(ctx, "unused"); err != nil || found {
		t.Fatal("inspection allocated state", found, err)
	}
	cp, err := s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("first"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", cp, r.Cursor, json.RawMessage(`{"seen":1}`), []Job{{Key: "done", Payload: json.RawMessage(`{"effect":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, "consumer", "done"); err != nil {
		t.Fatal(err)
	}
	p, err := s.ProducerCheckpoint(ctx, "producer")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerTransition(ctx, "producer", p, json.RawMessage(`{"value":1}`), []Job{{Key: "pending", Payload: json.RawMessage(`{"output":1}`)}}); err != nil {
		t.Fatal(err)
	}
	full, err := s.Capacity(ctx)
	if err != nil || full.Jobs.Rows != 2 || full.Jobs.RowLimit != 2 || full.Registrations.Rows != 2 || full.SlowestConsumerCursor != r.Cursor || full.Pressure != "exhausted" || !slices.Contains(full.Reasons, "jobs_rows") {
		t.Fatal("completed tombstone capacity hidden", full, err)
	}
	if full.DatabaseBytes == 0 || full.FilesystemBytes == 0 || full.FilesystemAvailableBytes > full.FilesystemBytes {
		t.Fatal("physical space not observed", full)
	}
	if _, err = s.Append(ctx, testEvent("second")); err != nil {
		t.Fatal(err)
	}
	full, err = s.Capacity(ctx)
	if err != nil || full.Events.Rows != 2 || full.FirstCursor != 1 || full.TipCursor != 2 || !slices.Contains(full.Reasons, "events_rows") {
		t.Fatal(full, err)
	}
	if retry, err := s.Append(ctx, testEvent("first")); err != nil || retry.Cursor != r.Cursor {
		t.Fatal("inspection changed full-store deduplication", retry, err)
	}
}

func TestCapacityWarnsBeforeExhaustionAndPersistsExactCharges(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 5}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if _, err = s.Append(ctx, testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	warning, err := s.Capacity(ctx)
	if err != nil || warning.Pressure != "warning" || !slices.Contains(warning.Reasons, "events_rows") {
		t.Fatal(warning, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reopened, err := s.Capacity(ctx)
	if err != nil || reopened.Events != warning.Events || reopened.TipCursor != warning.TipCursor {
		t.Fatal("capacity charges changed after reopen", reopened, err)
	}
}
