package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStatefulConsumerCommitsStateCursorAndWorkTogether(t *testing.T) {
	ctx := context.Background()
	options := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	old, err := s.Append(ctx, testEvent("historical"))
	if err != nil {
		t.Fatal(err)
	}
	start, err := s.Checkpoint(ctx, "rule")
	if err != nil || start.Cursor != old.Cursor || string(start.State) != "{}" {
		t.Fatalf("initial checkpoint: %+v %v", start, err)
	}
	r, err := s.Append(ctx, testEvent("counted-once"))
	if err != nil {
		t.Fatal(err)
	}
	state := json.RawMessage(`{"count":1}`)
	jobs := []Job{{Key: "fire-one", Payload: json.RawMessage(`{}`)}}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_state BEFORE UPDATE ON consumers BEGIN SELECT RAISE(ABORT,'injected state write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "rule", start, r.Cursor, state, jobs); err == nil {
		t.Fatal("expected transaction failure")
	}
	unchanged, err := s.Checkpoint(ctx, "rule")
	if err != nil || unchanged.Cursor != start.Cursor || unchanged.Version != start.Version || string(unchanged.State) != "{}" {
		t.Fatalf("state survived failed transaction: %+v %v", unchanged, err)
	}
	if pending, err := s.Pending(ctx, "rule", 10); err != nil || len(pending) != 0 {
		t.Fatal("work escaped rolled-back state", pending, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_state`); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "rule", start, r.Cursor, state, jobs); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "rule", start, r.Cursor, state, jobs); !errors.Is(err, ErrConflict) {
		t.Fatal("repeated input counted twice", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Checkpoint(ctx, "rule")
	if err != nil || next.Cursor != r.Cursor || next.Version != start.Version+1 || string(next.State) != string(state) {
		t.Fatalf("checkpoint lost on reopen: %+v %v", next, err)
	}
	// Timer-driven transitions keep the input cursor but still require a fresh
	// version. A stale timer or a competing consumer cannot emit twice.
	if err = s.Transition(ctx, "rule", next, next.Cursor, json.RawMessage(`{"count":0}`), []Job{{Key: "timer", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "rule", next, next.Cursor, json.RawMessage(`{"count":0}`), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale timer advanced checkpoint", err)
	}
	if _, err = s.Cursor(ctx, "rule"); !errors.Is(err, ErrConflict) {
		t.Fatal("stateless cursor accepted stateful consumer", err)
	}
	if err = s.Schedule(ctx, "rule", next.Cursor, next.Cursor+1, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stateless scheduler bypassed state", err)
	}
}

func TestConsumerStateCapacityRejectsWithoutAdvancingOrDroppingWork(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxConsumerStateBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkpoint, err := s.Checkpoint(ctx, "small")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("capacity"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "small", checkpoint, r.Cursor, json.RawMessage(`{"count":100}`), []Job{{Key: "must-not-escape", Payload: json.RawMessage(`{}`)}}); !errors.Is(err, ErrCapacity) {
		t.Fatal("expected state byte capacity", err)
	}
	after, err := s.Checkpoint(ctx, "small")
	if err != nil || after.Cursor != checkpoint.Cursor || after.Version != checkpoint.Version || string(after.State) != "{}" {
		t.Fatal("capacity advanced checkpoint", after, err)
	}
	if jobs, err := s.Pending(ctx, "small", 10); err != nil || len(jobs) != 0 {
		t.Fatal("capacity leaked work", jobs, err)
	}
}

func TestConcurrentTimersCommitOnlyOneStateVersion(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Append(ctx, testEvent("timer-input")); err != nil {
		t.Fatal(err)
	}
	before, err := s.Checkpoint(ctx, "timers")
	if err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Transition(ctx, "timers", before, before.Cursor, json.RawMessage(`{"fired":true}`), []Job{{Key: "one-fire", Payload: json.RawMessage(`{}`)}})
			if err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, err := s.Checkpoint(ctx, "timers")
	if err != nil || won.Load() != 1 || after.Version != before.Version+1 {
		t.Fatal("competing timer versions committed", won.Load(), after, err)
	}
	jobs, err := s.Pending(ctx, "timers", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
}

func TestUnreleasedPrototypeSchemaIsRejectedReadOnly(t *testing.T) {
	options := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(options.Directory, "journal.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(options); err == nil {
		reopened.Close()
		t.Fatal("silently imported discarded prototype schema")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unsupported schema rejection modified database", err)
	}
}

func TestStatefulCursorRollbackCannotReuseNewerRuleState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start, err := s.Checkpoint(ctx, "bound-state")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("already-counted"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "bound-state", start, r.Cursor, json.RawMessage(`{"count":1}`), nil); err != nil {
		t.Fatal(err)
	}
	// Recovering only a cursor must not make an old input advance a newer
	// aggregation state. A complete DB restore still needs target deduplication.
	if _, err = s.db.Exec(`UPDATE consumers SET cursor=0 WHERE name='bound-state'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Checkpoint(ctx, "bound-state"); !errors.Is(err, ErrConflict) {
		t.Fatal("cursor rollback exposed newer state for replay", err)
	}
	if err = s.Transition(ctx, "bound-state", Checkpoint{Cursor: 0, Version: 1}, r.Cursor, json.RawMessage(`{"count":2}`), []Job{{Key: "false-threshold", Payload: json.RawMessage(`{}`)}}); !errors.Is(err, ErrConflict) {
		t.Fatal("rolled-back input advanced state", err)
	}
	jobs, err := s.Pending(ctx, "bound-state", 10)
	if err != nil || len(jobs) != 0 {
		t.Fatal("rollback emitted an incorrect aggregate", jobs, err)
	}
}
