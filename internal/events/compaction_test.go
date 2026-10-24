package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestCompactionReleasesHotBudgetsWithoutReactivatingOrChangingWork(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events"), MaxJobs: 3, MaxTombstones: 20}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cp, err := s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("anchor"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Key: "done", Payload: json.RawMessage(`{ "value": 1 }`)}, {Key: "pending", Payload: json.RawMessage(`{"value":2}`)}}
	if err = s.Transition(ctx, "consumer", cp, r.Cursor, json.RawMessage(`{"count":1}`), jobs); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, "consumer", "done"); err != nil {
		t.Fatal(err)
	}
	p, err := s.ProducerCheckpoint(ctx, "producer")
	if err != nil {
		t.Fatal(err)
	}
	output := Job{Key: "done", Payload: json.RawMessage(`{ "value": 3 }`)}
	if err = s.ProducerTransition(ctx, "producer", p, json.RawMessage(`{"count":1}`), []Job{output}); err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerComplete(ctx, "producer", "done"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capacity(ctx)
	if err != nil || before.Jobs.Rows != 3 {
		t.Fatal(before, err)
	}
	// Consumer and producer namespaces share a key, but not its digest or source.
	n, err := s.CompactCompleted(ctx, 1)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	n, err = s.CompactCompleted(ctx, 10)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	after, err := s.Capacity(ctx)
	if err != nil || after.Jobs.Rows != 1 || after.Jobs.Bytes != 11 || after.Tombstones.Rows != 2 || after.Tombstones.Bytes <= 64 || after.Events != before.Events || after.Registrations != before.Registrations {
		t.Fatal(after, err)
	}
	pending, err := s.Pending(ctx, "consumer", 10)
	if err != nil || len(pending) != 1 || pending[0].Key != "pending" || string(pending[0].Payload) != `{"value":2}` || pending[0].EventCursor != r.Cursor {
		t.Fatal(pending, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal("compaction lost durable accounting", err)
	}
	for _, changed := range []bool{false, true} {
		current, err := s.Checkpoint(ctx, "consumer")
		if err != nil {
			t.Fatal(err)
		}
		retry := jobs[0]
		if changed {
			retry.Payload = json.RawMessage(`{"value":9}`)
		}
		err = s.Transition(ctx, "consumer", current, r.Cursor, current.State, []Job{retry})
		if !changed && err != nil || changed && !errors.Is(err, ErrConflict) {
			t.Fatal("consumer identity changed after compaction", changed, err)
		}
		producer, err := s.ProducerCheckpoint(ctx, "producer")
		if err != nil {
			t.Fatal(err)
		}
		retry = output
		if changed {
			retry.Payload = json.RawMessage(`{"value":9}`)
		}
		err = s.ProducerTransition(ctx, "producer", producer, producer.State, []Job{retry})
		if !changed && err != nil || changed && !errors.Is(err, ErrConflict) {
			t.Fatal("producer identity changed after compaction", changed, err)
		}
	}
	if err = s.Complete(ctx, "consumer", "done"); err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerComplete(ctx, "producer", "done"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ProducerPending(ctx, "producer", 10); err != nil || len(rows) != 0 {
		t.Fatal("completed producer work reactivated", rows, err)
	}
	current, err := s.ProducerCheckpoint(ctx, "producer")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerTransition(ctx, "producer", current, current.State, []Job{{Key: "new", Payload: json.RawMessage(`{"new":true}`)}}); err != nil {
		t.Fatal("reclaimed row/byte capacity was not reusable", err)
	}
	if n, err = s.CompactCompleted(ctx, 10); err != nil || n != 0 {
		t.Fatal("pending work was compacted", n, err)
	}
}

func TestCompactionRetainsConsumerSourceIdentityAfterCursorOnlyRollback(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Cursor(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Append(ctx, testEvent("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(ctx, testEvent("second"))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{Key: "effect", Payload: json.RawMessage(`{"effect":1}`)}
	if err = s.Schedule(ctx, "consumer", 0, first.Cursor, []Job{job}); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, "consumer", job.Key); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CompactCompleted(ctx, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = s.db.Exec(`UPDATE consumers SET cursor=0 WHERE name='consumer'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Schedule(ctx, "consumer", 0, first.Cursor, []Job{job}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Pending(ctx, "consumer", 10); err != nil || len(rows) != 0 {
		t.Fatal("old effect reactivated", rows, err)
	}
	if err = s.Schedule(ctx, "consumer", first.Cursor, second.Cursor, []Job{job}); !errors.Is(err, ErrConflict) {
		t.Fatal("same key/payload changed source event", err)
	}
}

func TestCompactionFailureAndDedupCapacityPreserveOriginalRows(t *testing.T) {
	for _, mode := range []string{"insert-failure", "delete-failure", "counter-failure", "row-capacity", "byte-capacity"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
			if mode == "row-capacity" {
				opts.MaxTombstones = 1
			}
			if mode == "byte-capacity" {
				opts.MaxTombstoneBytes = 1
			}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p, err := s.ProducerCheckpoint(ctx, "p")
			if err != nil {
				t.Fatal(err)
			}
			jobs := []Job{{Key: "a", Payload: json.RawMessage(`{"a":1}`)}, {Key: "b", Payload: json.RawMessage(`{"b":2}`)}}
			if err = s.ProducerTransition(ctx, "p", p, json.RawMessage(`{"v":1}`), jobs); err != nil {
				t.Fatal(err)
			}
			for _, j := range jobs {
				if err = s.ProducerComplete(ctx, "p", j.Key); err != nil {
					t.Fatal(err)
				}
			}
			var ddl string
			switch mode {
			case "insert-failure":
				ddl = `CREATE TRIGGER fail_compaction BEFORE INSERT ON completed_jobs BEGIN SELECT RAISE(ABORT,'injected compact insert'); END`
			case "delete-failure":
				ddl = `CREATE TRIGGER fail_compaction BEFORE DELETE ON producer_outbox BEGIN SELECT RAISE(ABORT,'injected compact delete'); END`
			case "counter-failure":
				ddl = `CREATE TRIGGER fail_compaction BEFORE UPDATE ON usage WHEN NEW.kind='outbox' BEGIN SELECT RAISE(ABORT,'injected compact counter'); END`
			}
			if ddl != "" {
				if _, err = s.db.Exec(ddl); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Capacity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n, err := s.CompactCompleted(ctx, 10)
			if mode == "row-capacity" {
				if err != nil || n != 1 {
					t.Fatal(n, err)
				}
				n, err = s.CompactCompleted(ctx, 10)
				if !errors.Is(err, ErrCapacity) || n != 0 {
					t.Fatal("dedup cap discarded an old key", n, err)
				}
			} else {
				if err == nil || n != 0 {
					t.Fatal("compaction failure committed partial rows", n, err)
				}
				after, e := s.Capacity(ctx)
				if e != nil || before.Jobs != after.Jobs || before.Tombstones != after.Tombstones {
					t.Fatal("failed batch changed counters", after, e)
				}
			}
			if ddl != "" {
				if _, err = s.db.Exec(`DROP TRIGGER fail_compaction`); err != nil {
					t.Fatal(err)
				}
				if n, err = s.CompactCompleted(ctx, 10); err != nil || n != 2 {
					t.Fatal("failure recovery did not retain both rows", n, err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(opts)
			if err != nil {
				t.Fatal("counter recovery", err)
			}
			defer s.Close()
			current, err := s.ProducerCheckpoint(ctx, "p")
			if err != nil || current.Version != 1 || !bytes.Equal(current.State, json.RawMessage(`{"v":1}`)) {
				t.Fatal("compaction changed producer state", current, err)
			}
			if err = s.ProducerTransition(ctx, "p", current, current.State, jobs); err != nil {
				t.Fatal("dedup retry failed after error", err)
			}
			if rows, err := s.ProducerPending(ctx, "p", 10); err != nil || len(rows) != 0 {
				t.Fatal("compaction failure reactivated jobs", rows, err)
			}
		})
	}
}

func TestCompactionConcurrencyBoundsAndClosedStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.ProducerCheckpoint(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	jobs := make([]Job, 20)
	for i := range jobs {
		jobs[i] = Job{Key: fmt.Sprint(i), Payload: json.RawMessage(`{}`)}
	}
	if err = s.ProducerTransition(ctx, "p", p, p.State, jobs); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ProducerComplete(ctx, "p", j.Key); err != nil {
				t.Error(err)
			}
			if _, err := s.CompactCompleted(ctx, 4); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err = s.CompactCompleted(ctx, 256); err != nil {
		t.Fatal(err)
	}
	c, err := s.Capacity(ctx)
	if err != nil || c.Tombstones.Rows != 20 || c.Jobs.Rows != 0 {
		t.Fatal(c, err)
	}
	for _, limit := range []int{0, -1, 257} {
		if _, err = s.CompactCompleted(ctx, limit); err == nil {
			t.Fatal("unbounded compaction", limit)
		}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.CompactCompleted(cancelCtx, 1); err == nil {
		t.Fatal("cancel ignored")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompactCompleted(ctx, 1); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestCompactionSQLiteFullRollsBackPayloadAndCounters(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.ProducerCheckpoint(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerTransition(ctx, "p", p, p.State, []Job{{Key: "a", Payload: json.RawMessage(`{"work":true}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerComplete(ctx, "p", "a"); err != nil {
		t.Fatal(err)
	}
	// Force an actual SQLite page allocation failure inside the same write
	// transaction, rather than returning a synthetic error from the API.
	if _, err = s.db.Exec(`CREATE TABLE compaction_space(value BLOB);CREATE TRIGGER full_compaction BEFORE INSERT ON completed_jobs BEGIN INSERT INTO compaction_space VALUES(zeroblob(1048576)); END`); err != nil {
		t.Fatal(err)
	}
	var pages int64
	if err = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.CompactCompleted(ctx, 16)
	var code interface{ Code() int }
	if n != 0 || !errors.As(err, &code) || code.Code()&255 != 13 {
		t.Fatal("expected actual SQLITE_FULL", n, err)
	}
	after, err := s.Capacity(ctx)
	if err != nil || after.Jobs != before.Jobs || after.Tombstones != before.Tombstones {
		t.Fatal("full transaction lost rows or identity", after, err)
	}
	if _, err = s.db.Exec(`PRAGMA max_page_count=2147483646;DROP TRIGGER full_compaction`); err != nil {
		t.Fatal(err)
	}
	if n, err = s.CompactCompleted(ctx, 16); err != nil || n != 1 {
		t.Fatal("full recovery failed", n, err)
	}
}

func TestCompactionSharesBatchesAndAccountsUTF8IdentityBytes(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if _, err = s.Cursor(ctx, "消費"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(ctx, testEvent("event"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Key: "一", Payload: json.RawMessage(`{}`)}, {Key: "二", Payload: json.RawMessage(`{}`)}, {Key: "三", Payload: json.RawMessage(`{}`)}}
	if err = s.Schedule(ctx, "消費", 0, r.Cursor, jobs); err != nil {
		t.Fatal(err)
	}
	p, err := s.ProducerCheckpoint(ctx, "生成")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProducerTransition(ctx, "生成", p, p.State, jobs); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if err = s.Complete(ctx, "消費", j.Key); err != nil {
			t.Fatal(err)
		}
		if err = s.ProducerComplete(ctx, "生成", j.Key); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CompactCompleted(ctx, 4); err != nil || n != 4 {
		t.Fatal(n, err)
	}
	for _, kind := range []int{0, 1} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM completed_jobs WHERE kind=?`, kind).Scan(&count); err != nil || count != 2 {
			t.Fatal("one namespace starved", kind, count, err)
		}
	}
	c, err := s.Capacity(ctx)
	if err != nil || c.Tombstones.Bytes != 200 {
		t.Fatal("UTF8 bytes counted as characters", c, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal("reopen accounting", err)
	}
}
