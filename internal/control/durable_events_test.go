package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
)

func attachJournal(t *testing.T, e *Engine, maximum int64) *events.Store {
	t.Helper()
	j, err := events.Open(events.Options{Directory: filepath.Join(e.store.Dir, "events"), MaxEvents: maximum})
	if err != nil {
		t.Fatal(err)
	}
	e.journal = j
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestConfigCommitAndEventOutboxRecoverTogether(t *testing.T) {
	ctx := context.Background()
	e, n := fixture(t)
	r := request(e, "durable")
	if _, err := e.Commit(ctx, r); err != nil {
		t.Fatal(err)
	}
	c, err := e.store.Load()
	if err != nil || len(c.EventOutbox) != 1 || c.EventOutbox[0].Revision != c.Revision || c.EventOutbox[0].Type != "committed" {
		t.Fatalf("configuration was acknowledged without its event: %+v, %v", c.EventOutbox, err)
	}
	// Model a crash after the journal ACK but before clearing the producer
	// outbox: recovery must retry the stable ID rather than inventing an event.
	recovered := New(e.store, c, n)
	j := attachJournal(t, recovered, 0)
	if err := recovered.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Save(c); err != nil {
		t.Fatal(err)
	}
	recovered = New(e.store, c, n)
	recovered.journal = j
	if err := recovered.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := j.List(ctx, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Event.ID != c.EventOutbox[0].ID || rows[0].Event.Attributes["revision"] != "2" {
		t.Fatalf("duplicate or missing recovered event: %+v, %v", rows, err)
	}
	if _, err = recovered.Commit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if pending, err := e.store.Load(); err != nil || len(pending.EventOutbox) != 0 {
		t.Fatalf("pending after replay: %+v %v", pending.EventOutbox, err)
	}
}

func TestJournalFullPreservesProducerOutboxAndReservesRollback(t *testing.T) {
	ctx := context.Background()
	e, n := fixture(t)
	j := attachJournal(t, e, 1)
	if _, err := e.Commit(ctx, request(e, "first")); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < config.MaxEventOutbox-2; i++ {
		if _, err := e.Commit(ctx, request(e, fmt.Sprintf("queued-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.FlushEvents(ctx); !errors.Is(err, events.ErrCapacity) {
		t.Fatalf("expected capacity failure: %v", err)
	}
	if c, err := e.store.Load(); err != nil || len(c.EventOutbox) != config.MaxEventOutbox-2 {
		t.Fatalf("lost pending history: %d %v", len(c.EventOutbox), err)
	}
	trial := request(e, "trial")
	trial.ConfirmTimeoutSeconds = 5
	if _, err := e.Commit(ctx, trial); err != nil {
		t.Fatal(err)
	}
	if err := e.Expire(ctx, e.State().Confirmation.Deadline.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	c, err := e.store.Load()
	if err != nil || len(c.EventOutbox) != config.MaxEventOutbox || c.EventOutbox[len(c.EventOutbox)-1].Type != "rolled_back_timeout" {
		t.Fatalf("rollback lost its reserved durable event: %+v %v", c.EventOutbox, err)
	}
	before := len(n.calls)
	if _, err := e.Commit(ctx, request(e, "over-capacity")); !errors.Is(err, events.ErrCapacity) {
		t.Fatalf("expected pre-effect backpressure: %v", err)
	}
	if len(n.calls) != before {
		t.Fatal("capacity rejection mutated kernel")
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j = attachJournal(t, e, 1000)
	if err = e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := j.List(ctx, 0, 100)
	if err != nil || len(rows) != config.MaxEventOutbox+1 {
		t.Fatalf("history after capacity recovery: %d %v", len(rows), err)
	}
}

func TestFailedCandidateAndPendingJournalNeverPublishCommitEvent(t *testing.T) {
	e, n := fixture(t)
	n.fail[2] = true
	if _, err := e.Commit(context.Background(), request(e, "failed")); err == nil {
		t.Fatal("expected apply failure")
	}
	c, err := e.store.Load()
	if err != nil || len(c.EventOutbox) != 0 {
		t.Fatalf("failed candidate became a committed observation: %+v %v", c.EventOutbox, err)
	}
}
