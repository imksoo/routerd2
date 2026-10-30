package events

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/retention"
)

func TestRetentionPreservesUnreadWorkAndRestartCursor(t *testing.T) {
	ctx := context.Background()
	options := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	cp, err := s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Append(ctx, testEvent("pending"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", cp, first.Cursor, json.RawMessage(`{"seen":1}`), []Job{{Key: "pending", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	last, err := s.Append(ctx, testEvent("unread"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxAgeSeconds: 1}, nil); err != nil || n != 0 {
		t.Fatal("removed pending or unread input", n, err)
	}
	if err = s.Complete(ctx, "consumer", "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompactCompleted(ctx, 64); err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxAgeSeconds: 1}, nil); err != nil || n != 0 {
		t.Fatal("removed current state source without pending work", n, err)
	}
	cp, err = s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", cp, last.Cursor, json.RawMessage(`{"seen":2}`), nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, "consumer", "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompactCompleted(ctx, 64); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ArchiveEvents(ctx, 16); err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxAgeSeconds: 1}, nil); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err = s.VacuumHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	cp, err = s.Checkpoint(ctx, "consumer")
	if err != nil || cp.Cursor != last.Cursor || string(cp.State) != `{"seen":2}` {
		t.Fatal(cp, err)
	}
	if err = s.Transition(ctx, "consumer", cp, cp.Cursor, cp.State, []Job{{Key: "timer", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal("retention removed current timer source", err)
	}
	cp, err = s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Append(ctx, testEvent("next"))
	if err != nil || next.Cursor <= last.Cursor {
		t.Fatal(next, err)
	}
	if err = s.Transition(ctx, "consumer", cp, next.Cursor, cp.State, nil); err != nil {
		t.Fatal(err)
	}
	c, err := s.Capacity(ctx)
	if err != nil || c.History.Rows != 2 || c.Tombstones.Rows != 1 {
		t.Fatal(c, err)
	}
}

func TestRetentionArchiveFailureAllowsAppendAndKeepsCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Append(ctx, testEvent("original"))
	if err != nil {
		t.Fatal(err)
	}
	anchor := testEvent("anchor")
	anchor.ObservedAt = time.Now()
	if _, err = s.Append(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	archiveFailure := errors.New("receiver path unavailable")
	deliver := func(ctx context.Context, record Record, body []byte) error {
		if record.Cursor != first.Cursor || !json.Valid(body) {
			t.Fatal("changed original", record)
		}
		// This uses the same connection pool while the before-delete IO is pending.
		fresh := testEvent("accepted-during-archive-failure")
		fresh.ObservedAt = time.Now()
		if _, err := s.Append(ctx, fresh); err != nil {
			t.Fatal("archive held journal connection", err)
		}
		return archiveFailure
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxAgeSeconds: 1}, deliver); !errors.Is(err, archiveFailure) || n != 0 {
		t.Fatal(n, err)
	}
	records, err := s.List(ctx, 0, 16)
	if err != nil || len(records) != 3 || records[0].Cursor != first.Cursor {
		t.Fatal(records, err)
	}
	c, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxBytes: c.History.Bytes - 1}, nil); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxRecords: 1}, nil); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	latest := records[2]
	// A new stateful consumer starts at the retained tip, then a timer may
	// create work immediately without receiving a new source event.
	if _, _, n, err := s.PruneHistory(ctx, 0, retention.Config{MaxBytes: 1}, nil); err != nil || n != 0 {
		t.Fatal("removed registration anchor", n, err)
	}
	cp, err := s.Checkpoint(ctx, "new-consumer")
	if err != nil || cp.Cursor != latest.Cursor {
		t.Fatal(cp, err)
	}
	if err = s.Transition(ctx, "new-consumer", cp, cp.Cursor, cp.State, []Job{{Key: "first-timer", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal("new timer lost its source", err)
	}
}
