package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProducerBootstrapIsIndependentOfHistoricalInput(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cp, err := s.ProducerCheckpoint(ctx, "derived")
	if err != nil || cp.Version != 0 || string(cp.State) != "{}" {
		t.Fatal(cp, err)
	}
	job := Job{Key: "initial", Payload: json.RawMessage(`{"asserted":true}`)}
	state := json.RawMessage(`{"counter":1}`)
	if err = s.ProducerTransition(ctx, "derived", cp, state, []Job{job}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.List(ctx, 0, 10); err != nil || len(rows) != 0 {
		t.Fatal("producer fabricated history", rows, err)
	}
	work, err := s.ProducerPending(ctx, "derived", 10)
	if err != nil || len(work) != 1 || work[0].Key != job.Key || work[0].StateVersion != 1 {
		t.Fatal(work, err)
	}
	// A same-named journal consumer has its own state and cannot execute or
	// acknowledge producer work through the consumer API.
	consumer, err := s.Checkpoint(ctx, "derived")
	if err != nil || consumer.Cursor != 0 || consumer.Version != 0 {
		t.Fatal(consumer, err)
	}
	if work, err := s.Pending(ctx, "derived", 10); err != nil || len(work) != 0 {
		t.Fatal(work, err)
	}
	if err = s.Complete(ctx, "derived", job.Key); !errors.Is(err, ErrConflict) {
		t.Fatal("consumer completed producer job", err)
	}
	if err = s.ProducerComplete(ctx, "derived", job.Key); err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerComplete(ctx, "derived", job.Key); err != nil {
		t.Fatal("completion not idempotent", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.ProducerCheckpoint(ctx, "derived")
	if err != nil || next.Version != 1 || !bytes.Equal(next.State, state) {
		t.Fatal("state lost across reopen", next, err)
	}
	if err = s.ProducerTransition(ctx, "derived", cp, state, []Job{job}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale version accepted", err)
	}
	// Stable output keys retain completed tombstones across a fresh state
	// transition; changing content beneath an old key is a conflict.
	if err = s.ProducerTransition(ctx, "derived", next, state, []Job{job}); err != nil {
		t.Fatal(err)
	}
	if work, err := s.ProducerPending(ctx, "derived", 10); err != nil || len(work) != 0 {
		t.Fatal("completed output reactivated", work, err)
	}
	next, err = s.ProducerCheckpoint(ctx, "derived")
	if err != nil {
		t.Fatal(err)
	}
	job.Payload = json.RawMessage(`{"asserted":false}`)
	if err = s.ProducerTransition(ctx, "derived", next, state, []Job{job}); !errors.Is(err, ErrConflict) {
		t.Fatal("changed output identity accepted", err)
	}
}

func TestProducerFailureAndSharedCapacityAreAtomic(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"sql-failure", "state-capacity", "job-capacity"} {
		t.Run(mode, func(t *testing.T) {
			opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
			if mode == "state-capacity" {
				opts.MaxConsumerStateBytes = 10
			}
			if mode == "job-capacity" {
				opts.MaxJobs = 1
			}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			consumer, err := s.Checkpoint(ctx, "consumer")
			if err != nil {
				t.Fatal(err)
			}
			cp, err := s.ProducerCheckpoint(ctx, "producer")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "job-capacity" {
				record, err := s.Append(ctx, testEvent("anchor"))
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Transition(ctx, "consumer", consumer, record.Cursor, json.RawMessage(`{}`), []Job{{Key: "occupied", Payload: json.RawMessage(`{}`)}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "sql-failure" {
				if _, err = s.db.Exec(`CREATE TRIGGER fail_producer BEFORE UPDATE ON producers BEGIN SELECT RAISE(ABORT,'injected state failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = s.ProducerTransition(ctx, "producer", cp, json.RawMessage(`{"asserted":true}`), []Job{{Key: "bootstrap", Payload: json.RawMessage(`{}`)}})
			if err == nil || mode != "sql-failure" && !errors.Is(err, ErrCapacity) {
				t.Fatal("expected atomic storage failure", err)
			}
			current, err := s.ProducerCheckpoint(ctx, "producer")
			if err != nil || current.Version != cp.Version || !bytes.Equal(current.State, cp.State) {
				t.Fatal("partial state committed", current, err)
			}
			if work, err := s.ProducerPending(ctx, "producer", 10); err != nil || len(work) != 0 {
				t.Fatal("partial output committed", work, err)
			}
			if mode == "sql-failure" {
				if _, err = s.db.Exec(`DROP TRIGGER fail_producer`); err != nil {
					t.Fatal(err)
				}
				if err = s.ProducerTransition(ctx, "producer", current, json.RawMessage(`{"asserted":true}`), []Job{{Key: "bootstrap", Payload: json.RawMessage(`{}`)}}); err != nil {
					t.Fatal("retry failed", err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			// Reopen validates the shared durable usage counters.
			s, err = Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProducerConcurrentClaimsAndBounds(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxConsumers: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cp, err := s.ProducerCheckpoint(ctx, "derived")
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.ProducerTransition(ctx, "derived", cp, json.RawMessage(`{"count":1}`), []Job{{Key: "once", Payload: json.RawMessage(`{}`)}})
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("concurrent claims committed", wins.Load())
	}
	cp, err = s.ProducerCheckpoint(ctx, "derived")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []json.RawMessage{nil, json.RawMessage(`invalid`), json.RawMessage(`"` + strings.Repeat("x", maxBody) + `"`)} {
		if err = s.ProducerTransition(ctx, "derived", cp, state, nil); err == nil {
			t.Fatal("invalid state accepted")
		}
	}
	if _, err = s.Checkpoint(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProducerCheckpoint(ctx, "over-budget"); !errors.Is(err, ErrCapacity) {
		t.Fatal("registration bypassed shared limit", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProducerCheckpoint(ctx, "derived"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err = s.ProducerTransition(ctx, "derived", cp, json.RawMessage(`{}`), nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err = s.ProducerPending(ctx, "derived", 10); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err = s.ProducerComplete(ctx, "derived", "once"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
