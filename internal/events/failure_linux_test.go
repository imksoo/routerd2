//go:build linux

package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSQLiteFullRollsBackEventUsageAndCursorOutbox(t *testing.T) {
	for _, mode := range []string{"journal", "outbox"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.Cursor(ctx, "consumer"); err != nil {
				t.Fatal(err)
			}
			var record Record
			if mode == "outbox" {
				record, err = s.Append(ctx, testEvent("source"))
				if err != nil {
					t.Fatal(err)
				}
			}
			var pages int64
			if err = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
				t.Fatal(err)
			}
			if mode == "journal" {
				event := testEvent("full")
				event.Attributes = map[string]string{}
				for i := range 8 {
					event.Attributes[fmt.Sprint(i)] = strings.Repeat("x", 4000)
				}
				_, err = s.Append(ctx, event)
			} else {
				payload, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 32000)})
				err = s.Schedule(ctx, "consumer", 0, record.Cursor, []Job{{Key: "work", Payload: payload}})
			}
			var code interface{ Code() int }
			if !errors.As(err, &code) || code.Code()&255 != 13 {
				t.Fatalf("expected actual SQLITE_FULL, got %v", err)
			}
			cursor, err := s.Cursor(ctx, "consumer")
			if err != nil || cursor != 0 {
				t.Fatal("failed write advanced cursor", cursor, err)
			}
			pending, err := s.Pending(ctx, "consumer", 256)
			if err != nil || len(pending) != 0 {
				t.Fatal("failed write left pending work", pending, err)
			}
			if _, err = s.db.Exec(`PRAGMA max_page_count=2147483646`); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Append(ctx, testEvent("after-recovery")); err != nil {
				t.Fatal("disk recovery left usage or writer poisoned", err)
			}
		})
	}
}

func TestPrivateStoreRejectsLinksUnsafeDirectoriesAndFIFO(t *testing.T) {
	for _, kind := range []string{"directory-symlink", "unsafe-directory", "lock-symlink", "lock-hardlink", "db-symlink", "wal-symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "events")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(base, "foreign")
			sentinel := []byte("foreign state must survive")
			if err := os.WriteFile(foreign, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "directory-symlink":
				link := filepath.Join(base, "link")
				err = os.Symlink(dir, link)
				dir = link
			case "unsafe-directory":
				err = os.Chmod(dir, 0770)
			case "lock-symlink":
				err = os.Symlink(foreign, filepath.Join(dir, "store.lock"))
			case "lock-hardlink":
				err = os.Link(foreign, filepath.Join(dir, "store.lock"))
			case "db-symlink":
				err = os.Symlink(foreign, filepath.Join(dir, "journal.sqlite3"))
			case "wal-symlink":
				err = os.Symlink(foreign, filepath.Join(dir, "journal.sqlite3-wal"))
			case "fifo":
				err = unix.Mkfifo(filepath.Join(dir, "journal.sqlite3"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(Options{Directory: dir})
			if err == nil {
				s.Close()
				t.Fatal("unsafe store path accepted")
			}
			got, err := os.ReadFile(foreign)
			if err != nil || string(got) != string(sentinel) {
				t.Fatal("foreign state changed", err)
			}
		})
	}
}

func TestSingleWriterAndConcurrentIdempotentPublication(t *testing.T) {
	ctx := context.Background()
	opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if second, err := Open(opts); err == nil {
		second.Close()
		t.Fatal("two writers acquired one journal")
	}
	var wg sync.WaitGroup
	failures := make(chan error, 80)
	for i := range 80 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Append(ctx, testEvent(fmt.Sprint(i%20)))
			if err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	rows, err := s.List(ctx, 0, 256)
	if err != nil || len(rows) != 20 {
		t.Fatal("duplicate race changed event cardinality", len(rows), err)
	}
}

func TestCursorCannotSkipAndOutboxCapacityIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxJobs: 1, MaxConsumers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Cursor(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cursor(ctx, "another"); !errors.Is(err, ErrCapacity) {
		t.Fatal("unbounded consumer registry", err)
	}
	first, err := s.Append(ctx, testEvent("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(ctx, testEvent("two"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Schedule(ctx, "consumer", 0, second.Cursor, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("cursor skipped an event", err)
	}
	jobs := []Job{{Key: "one", Payload: json.RawMessage(`{}`)}, {Key: "two", Payload: json.RawMessage(`{}`)}}
	if err = s.Schedule(ctx, "consumer", 0, first.Cursor, jobs); !errors.Is(err, ErrCapacity) {
		t.Fatal("outbox capacity failed open", err)
	}
	if pending, err := s.Pending(ctx, "consumer", 256); err != nil || len(pending) != 0 {
		t.Fatal("partial outbox commit", pending, err)
	}
	if cursor, err := s.Cursor(ctx, "consumer"); err != nil || cursor != 0 {
		t.Fatal("partial cursor commit", cursor, err)
	}
	if err = s.Schedule(ctx, "consumer", 0, first.Cursor, jobs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE consumers SET cursor=999 WHERE name='consumer'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cursor(ctx, "consumer"); !errors.Is(err, ErrConflict) {
		t.Fatal("restored cursor ahead of journal accepted", err)
	}
}

func TestReplacementConnectionRetainsDurabilityAndForeignKeys(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxIdleConns(0) // force a fresh connection for each following call
	for range 3 {
		var synchronous, foreignKeys int
		if err = s.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
			t.Fatal("replacement connection lost FULL synchronous mode", synchronous, err)
		}
		if err = s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
			t.Fatal("replacement connection lost foreign key enforcement", foreignKeys, err)
		}
	}
}
