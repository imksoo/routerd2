package events

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestArchiveReusesHotBudgetWhileAllHistoryAndSourceCursorsRemainReadable(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 2, MaxHistoryEvents: 3}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	cp, err := s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	first := testEvent("one")
	first.Attributes["data"] = strings.Repeat("a", 2000)
	r, err := s.Append(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := testEvent("two")
	second.Attributes["data"] = strings.Repeat("b", 2000)
	two, err := s.Append(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", cp, r.Cursor, json.RawMessage(`{"count":1}`), []Job{{Key: "pending", Payload: json.RawMessage(`{"effect":1}`)}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ArchiveEvents(ctx, 16); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	after, err := s.Capacity(ctx)
	if err != nil || after.Events.Rows != 0 || after.Events.Bytes != 0 || after.History != before.History || after.ArchivedEvents.Rows != 2 || after.ArchivedEvents.Bytes >= after.History.Bytes || after.FirstCursor != r.Cursor || after.TipCursor != two.Cursor || after.Jobs != before.Jobs || after.Registrations != before.Registrations {
		t.Fatal(after, err)
	}
	if rows, err := s.List(ctx, 0, 16); err != nil || !reflect.DeepEqual(rows, []Record{r, two}) {
		t.Fatal("archive changed complete logical history", rows, err)
	}
	if retry, err := s.Append(ctx, first); err != nil || !reflect.DeepEqual(retry, r) {
		t.Fatal("archived retry was not acknowledged exactly", retry, err)
	}
	changed := first
	changed.Topic = "changed"
	if _, err = s.Append(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("archived identity was reused", err)
	}
	pending, err := s.Pending(ctx, "consumer", 16)
	if err != nil || len(pending) != 1 || pending[0].EventCursor != r.Cursor {
		t.Fatal("pending reference lost", pending, err)
	}
	current, err := s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", current, current.Cursor, current.State, []Job{{Key: "timer", Payload: json.RawMessage(`{"timer":true}`)}}); err != nil {
		t.Fatal("timer lost its archived source anchor", err)
	}
	third, err := s.Append(ctx, testEvent("three"))
	if err != nil || third.Cursor != two.Cursor+1 {
		t.Fatal("hot capacity or monotonic tip was lost", third, err)
	}
	tip, err := s.Cursor(ctx, "new")
	if err != nil || tip != third.Cursor {
		t.Fatal("new consumer did not start at logical tip", tip, err)
	}
	if n, err := s.ArchiveEvents(ctx, 16); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = s.Append(ctx, testEvent("four")); !errors.Is(err, ErrCapacity) {
		t.Fatal("archive bypassed whole-history bound", err)
	}
	if retry, err := s.Append(ctx, first); err != nil || retry.Cursor != r.Cursor {
		t.Fatal("global full store lost old dedup ACK", retry, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(opts)
	if err != nil {
		t.Fatal("archive reopen", err)
	}
	if rows, err := s.List(ctx, 0, 16); err != nil || !reflect.DeepEqual(rows, []Record{r, two, third}) {
		t.Fatal(rows, err)
	}
	current, err = s.Checkpoint(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "consumer", current, two.Cursor, current.State, nil); err != nil {
		t.Fatal("slow consumer could not advance through archived input", err)
	}
}

func TestArchiveExportKeepsSnapshotDuringConcurrentTierMovement(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"one", "two"} {
		if _, err = s.Append(ctx, testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	writer := &archiveHook{hook: func() {
		if n, err := s.ArchiveEvents(ctx, 16); err != nil || n != 2 {
			t.Fatal(n, err)
		}
		if _, err = s.Append(ctx, testEvent("later")); err != nil {
			t.Fatal(err)
		}
	}}
	summary, err := s.ExportHistory(ctx, writer)
	if err != nil || summary.Records != 2 || summary.Header.TipCursor != 2 {
		t.Fatal(summary, err)
	}
	if verified, err := VerifyHistory(ctx, bytes.NewReader(writer.Bytes())); err != nil || verified != summary {
		t.Fatal(verified, err)
	}
	var out bytes.Buffer
	if final, err := s.ExportHistory(ctx, &out); err != nil || final.Records != 3 {
		t.Fatal("mixed hot/archive export was incomplete", final, err)
	}
}

func TestArchiveFailureAndSeparateBudgetsPreserveEveryAcceptedBody(t *testing.T) {
	for _, mode := range []string{"insert", "clear", "counter", "row-capacity", "byte-capacity", "sqlite-full"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
			if mode == "row-capacity" {
				opts.MaxArchivedEvents = 1
			}
			if mode == "byte-capacity" {
				opts.MaxArchivedEventBytes = 1
			}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, id := range []string{"one", "two"} {
				if _, err = s.Append(ctx, testEvent(id)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Capacity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			history, err := s.List(ctx, 0, 16)
			if err != nil {
				t.Fatal(err)
			}
			var ddl string
			switch mode {
			case "insert":
				ddl = `CREATE TRIGGER fail_archive BEFORE INSERT ON event_archive BEGIN SELECT RAISE(ABORT,'injected archive insert'); END`
			case "clear":
				ddl = `CREATE TRIGGER fail_archive BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'injected archive clear'); END`
			case "counter":
				ddl = `CREATE TRIGGER fail_archive BEFORE UPDATE ON usage WHEN NEW.kind='events' BEGIN SELECT RAISE(ABORT,'injected archive counter'); END`
			case "sqlite-full":
				ddl = `CREATE TABLE archive_space(value BLOB);CREATE TRIGGER fail_archive BEFORE INSERT ON event_archive BEGIN INSERT INTO archive_space VALUES(zeroblob(1048576)); END`
			}
			if ddl != "" {
				if _, err = s.db.Exec(ddl); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "sqlite-full" {
				var pages int
				if err = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
					t.Fatal(err)
				}
			}
			n, err := s.ArchiveEvents(ctx, 16)
			if mode == "row-capacity" {
				if err != nil || n != 1 {
					t.Fatal(n, err)
				}
				n, err = s.ArchiveEvents(ctx, 16)
				if n != 0 || !errors.Is(err, ErrCapacity) {
					t.Fatal(n, err)
				}
			} else {
				if err == nil || n != 0 {
					t.Fatal("failure committed partial tier movement", n, err)
				}
				if mode == "sqlite-full" {
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code()&255 != 13 {
						t.Fatal("expected actual SQLITE_FULL", err)
					}
				}
				after, e := s.Capacity(ctx)
				if e != nil || after.Events != before.Events || after.History != before.History || after.ArchivedEvents != before.ArchivedEvents {
					t.Fatal("failure changed accounting", after, e)
				}
			}
			if rows, err := s.List(ctx, 0, 16); err != nil || !reflect.DeepEqual(rows, history) {
				t.Fatal("accepted body lost", rows, err)
			}
			if ddl != "" {
				if _, err = s.db.Exec(`PRAGMA max_page_count=2147483646;DROP TRIGGER fail_archive`); err != nil {
					t.Fatal(err)
				}
				if n, err = s.ArchiveEvents(ctx, 16); err != nil || n != 2 {
					t.Fatal("archive recovery lost source", n, err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(opts)
			if err != nil {
				t.Fatal("archive accounting reopen", err)
			}
			defer s.Close()
			if rows, err := s.List(ctx, 0, 16); err != nil || !reflect.DeepEqual(rows, history) {
				t.Fatal(rows, err)
			}
		})
	}
}

func TestArchivedCorruptionCannotBeReadOrAcknowledgedAsDurable(t *testing.T) {
	for _, mode := range []string{"gzip", "hash", "length", "trailing", "multistream", "bomb"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			original := testEvent("original")
			if _, err = s.Append(ctx, original); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ArchiveEvents(ctx, 16); err != nil {
				t.Fatal(err)
			}
			var body []byte
			if err = s.db.QueryRow(`SELECT body FROM event_archive`).Scan(&body); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "gzip":
				body = []byte("bad gzip")
			case "hash":
				_, err = s.db.Exec(`UPDATE event_archive SET digest=zeroblob(32)`)
			case "length":
				_, err = s.db.Exec(`UPDATE event_archive SET original_bytes=original_bytes+1`)
			case "trailing":
				body = append(body, 0)
			case "multistream":
				body = append(body, body...)
			case "bomb":
				var b bytes.Buffer
				w := gzip.NewWriter(&b)
				if _, err = w.Write(bytes.Repeat([]byte("a"), 1<<20)); err != nil {
					t.Fatal(err)
				}
				if err = w.Close(); err != nil {
					t.Fatal(err)
				}
				body = b.Bytes()
				_, err = s.db.Exec(`UPDATE event_archive SET original_bytes=65536`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != "hash" && mode != "length" {
				if _, err = s.db.Exec(`UPDATE event_archive SET body=?`, body); err != nil {
					t.Fatal(err)
				}
			}
			if rows, err := s.List(ctx, 0, 16); err == nil {
				t.Fatal("corrupt archive read as valid", rows)
			}
			if _, err = s.Append(ctx, original); err == nil {
				t.Fatal("lost/corrupt body acknowledged as durable")
			}
			var out bytes.Buffer
			if _, err = s.ExportHistory(ctx, &out); err == nil {
				t.Fatal("corrupt archive exported successfully")
			}
			var hot int
			if err = s.db.QueryRow(`SELECT count(*) FROM events WHERE length(body)>0`).Scan(&hot); err != nil || hot != 0 {
				t.Fatal("retry manufactured replacement history", hot, err)
			}
		})
	}
}

func TestArchiveConcurrentAppendRetryAndBounds(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 4 {
				if _, err := s.Append(ctx, testEvent(fmt.Sprintf("%d/%d", i, j))); err != nil {
					t.Error(err)
				}
				if _, err := s.ArchiveEvents(ctx, 4); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if _, err = s.ArchiveEvents(ctx, 256); err != nil {
		t.Fatal(err)
	}
	c, err := s.Capacity(ctx)
	if err != nil || c.History.Rows != 64 || c.ArchivedEvents.Rows != 64 || c.Events.Rows != 0 || c.TipCursor != 64 {
		t.Fatal(c, err)
	}
	for _, limit := range []int{0, -1, 257} {
		if _, err = s.ArchiveEvents(ctx, limit); err == nil {
			t.Fatal("unbounded archive", limit)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.ArchiveEvents(cancelled, 1); err == nil {
		t.Fatal("cancel ignored")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ArchiveEvents(ctx, 1); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestWholeHistoryByteLimitIsAtomicAfterTierMovement(t *testing.T) {
	ctx := context.Background()
	e := testEvent("one")
	body, err := encodeEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxHistoryBytes: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ArchiveEvents(ctx, 16); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(ctx, testEvent("two")); !errors.Is(err, ErrCapacity) {
		t.Fatal("whole-history byte cap bypassed", err)
	}
	after, err := s.Capacity(ctx)
	if err != nil || before.History != after.History || before.Events != after.Events || before.ArchivedEvents != after.ArchivedEvents {
		t.Fatal("failed append leaked reservations", after, err)
	}
}

func TestArchivedBodyPagesCanBeReusedByNewEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := testEvent("large")
	for i := range 8 {
		first.Attributes[fmt.Sprint(i)] = strings.Repeat("a", 4000)
	}
	if _, err = s.Append(ctx, first); err != nil {
		t.Fatal(err)
	}
	var original, freeBefore, freeAfter int
	if err = s.db.QueryRow(`PRAGMA page_count`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freeBefore); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ArchiveEvents(ctx, 16); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err = s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freeAfter); err != nil || freeAfter <= freeBefore {
		t.Fatal("archival did not release reusable payload pages", freeBefore, freeAfter, err)
	}
	second := testEvent("new")
	for i := range 4 {
		second.Attributes[fmt.Sprint(i)] = strings.Repeat("b", 3000)
	}
	if _, err = s.Append(ctx, second); err != nil {
		t.Fatal("archived hot capacity was not reusable", err)
	}
	var pages, freeFinal int
	if err = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freeFinal); err != nil || pages > original || freeFinal >= freeAfter {
		t.Fatal("new event did not reuse archived payload pages", original, pages, freeAfter, freeFinal, err)
	}
	t.Logf("archive released %d pages; new event reused %d without DB growth", freeAfter-freeBefore, freeAfter-freeFinal)
}
