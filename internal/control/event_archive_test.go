package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveWriteFailureCannotBeClearedByInspectionOrConfirmed(t *testing.T) {
	e, _ := fixture(t)
	j := attachJournal(t, e, 0)
	ctx := context.Background()
	trial := request(e, "archive-trial")
	trial.ConfirmTimeoutSeconds = 60
	state, err := e.Commit(ctx, trial)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(e.store.Dir, "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_archive BEFORE INSERT ON event_archive BEGIN SELECT RAISE(ABORT,'injected archive failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = e.maintainEventStore(ctx, archiveEventWork); err == nil {
		t.Fatal("archive write failure was hidden")
	}
	if err = e.RefreshEventStore(ctx); err != nil {
		t.Fatal(err)
	}
	h := e.health()
	if h.Healthy || h.EventJournal.Storage.ArchiveError == "" || !strings.Contains(h.EventJournal.Storage.LastError, "archive") {
		t.Fatal("inspection cleared failed archival write", h)
	}
	if _, err = e.Confirm(ConfirmRequest{Revision: state.Revision, RequestID: trial.RequestID}); err == nil {
		t.Fatal("failed archive allowed trial confirmation")
	}
	if rows, err := j.List(ctx, 0, 16); err != nil || len(rows) != 1 {
		t.Fatal("archive failure lost accepted history", rows, err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_archive`); err != nil {
		t.Fatal(err)
	}
	if err = e.maintainEventStore(ctx, archiveEventWork); err != nil {
		t.Fatal(err)
	}
	if err = e.RefreshEventStore(ctx); err != nil {
		t.Fatal(err)
	}
	h = e.health()
	if !h.Healthy || h.EventJournal.Storage.ArchiveError != "" || h.EventJournal.Storage.Capacity.ArchivedEvents.Rows != 1 || h.EventJournal.Storage.Capacity.Events.Rows != 0 {
		t.Fatal(h)
	}
	if _, err = e.Confirm(ConfirmRequest{Revision: state.Revision, RequestID: trial.RequestID}); err != nil {
		t.Fatal("recovered archive prevented confirmation", err)
	}
}
