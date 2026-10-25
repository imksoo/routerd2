//go:build linux

package events

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFormatInspectionUsesLiveReadOnlySchemaAndDoesNotAdvanceState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(ctx, testEvent("format-inspection")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cursor(ctx, "existing-consumer"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var changesBefore, changesAfter int64
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	got, err := s.InspectFormat(ctx)
	if err != nil || got.ApplicationID != applicationID || got.Version != SchemaVersion || got.JournalMode != "wal" || got.Objects < 10 || len(got.SchemaSHA256) != 64 {
		t.Fatal("invalid actual schema observation", got, err)
	}
	t.Logf("actual event format=%d objects=%d schema_sha256=%s", got.Version, got.Objects, got.SchemaSHA256)
	// Freeze the persisted contract independently of the DDL-derived expectation:
	// changing required SQL cannot silently keep schema6 and its ABI claim.
	if SchemaVersion != 6 || got.SchemaSHA256 != "2bc436d24ae4fcc8ce21c43419888b4619f7cd6199893e771c21342c860570e4" {
		t.Fatal("schema definition changed without a new qualified version contract")
	}
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	if changesBefore != changesAfter {
		t.Fatal("inspection changed durable SQL state")
	}
	if _, err := s.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	after, err := s.Capacity(ctx)
	if err != nil || after.TipCursor != before.TipCursor || after.SlowestConsumerCursor != before.SlowestConsumerCursor || after.Registrations != before.Registrations || after.Events != before.Events || after.Jobs != before.Jobs {
		t.Fatal("inspection advanced or registered state", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.InspectFormat(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled inspection proceeded", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectFormat(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed store returned cached format", err)
	}
}

func TestFormatInspectionRefusesHeaderAndSchemaChangesWithSameLiveStore(t *testing.T) {
	for _, ddl := range []string{`PRAGMA application_id=7`, `PRAGMA user_version=5`, `ALTER TABLE events ADD COLUMN foreign_owner TEXT`, `DROP INDEX pending_work`, `CREATE TRIGGER extra_effect AFTER INSERT ON events BEGIN SELECT 1; END`, `CREATE TABLE unrelated(value TEXT)`, `CREATE TABLE sqlitex_unexpected(value TEXT)`} {
		t.Run(ddl, func(t *testing.T) {
			s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.InspectFormat(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InspectFormat(context.Background()); err == nil {
				t.Fatal("source constants/cached result hid changed database format")
			}
		})
	}
}

func TestFormatInspectionDetectsLostOrReplacedDatabasePath(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(s.options.Directory, "journal.sqlite3")
	original := path + ".original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(path)
		if err := os.Rename(original, path); err != nil {
			t.Error(err)
		}
	}()
	if _, err := s.InspectFormat(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lost established file treated as cached database", err)
	}
	if err := os.WriteFile(path, []byte("replacement must not be trusted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectFormat(context.Background()); err == nil {
		t.Fatal("different inode hidden by old SQLite connection")
	}
}

func TestFormatInspectionSeesCoherentMetadataDuringEventWrites(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	initial, err := s.InspectFormat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := s.Append(ctx, testEvent(fmt.Sprintf("concurrent-format-%d", i))); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	for i := 0; i < 20; i++ {
		if got, err := s.InspectFormat(ctx); err != nil || got != initial {
			t.Errorf("metadata changed with event bodies: %+v %v", got, err)
			break
		}
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestFormatInspectionDeadlineWhileTheSQLiteConnectionIsBusy(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, inspectErr := s.InspectFormat(ctx)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(inspectErr, context.DeadlineExceeded) {
		t.Fatal("busy read ignored its caller deadline", inspectErr)
	}
	if _, err := s.InspectFormat(context.Background()); err != nil {
		t.Fatal("timed out read poisoned the actual store", err)
	}
}

// Existing injected operational faults do not redefine the core codec. A full
// update preflight is stricter and refuses all unexpected schema objects.
func TestOperationalOpenChecksRequiredDDLButQualificationRequiresExactCatalog(t *testing.T) {
	opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER temporary_write_failure BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'controlled fixture fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenExisting(opts)
	if err != nil {
		t.Fatal("operational core reader changed fault-recovery semantics", err)
	}
	defer s.Close()
	if _, err := s.InspectFormat(context.Background()); err == nil {
		t.Fatal("unknown effect trigger qualified as supported update state")
	}
}
