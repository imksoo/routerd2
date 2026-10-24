package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/events"
)

func TestEventCompactionFailurePersistsAcrossFreshInspectionUntilMaintenanceRecovers(t *testing.T) {
	e, _ := fixture(t)
	j := attachJournal(t, e, 0)
	ctx := context.Background()
	trial := request(e, "compact-trial")
	trial.ConfirmTimeoutSeconds = 60
	state, err := e.Commit(ctx, trial)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := j.ProducerCheckpoint(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	job := events.Job{Key: "done", Payload: json.RawMessage(`{"output":true}`)}
	if err = j.ProducerTransition(ctx, "fixture", cp, cp.State, []events.Job{job}); err != nil {
		t.Fatal(err)
	}
	if err = j.ProducerComplete(ctx, "fixture", job.Key); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(e.store.Dir, "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_compaction BEFORE INSERT ON completed_jobs BEGIN SELECT RAISE(ABORT,'injected compaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = e.MaintainEventStore(ctx); err == nil {
		t.Fatal("maintenance failure hidden")
	}
	if h := e.health(); h.Healthy || h.EventJournal.Storage.CompactionError == "" || !strings.Contains(h.EventJournal.Storage.LastError, "compaction") {
		t.Fatal(h)
	}
	if err = e.RefreshEventStore(ctx); err != nil {
		t.Fatal("inspection itself failed", err)
	}
	if h := e.health(); h.Healthy || h.EventJournal.Storage.CompactionError == "" {
		t.Fatal("fresh read cleared failed write alarm", h)
	}
	if _, err = e.Confirm(ConfirmRequest{Revision: state.Revision, RequestID: trial.RequestID}); err == nil {
		t.Fatal("failed maintenance allowed trial confirmation")
	}
	if _, err = db.Exec(`DROP TRIGGER fail_compaction`); err != nil {
		t.Fatal(err)
	}
	if err = e.MaintainEventStore(ctx); err != nil {
		t.Fatal(err)
	}
	h := e.health()
	if !h.Healthy || h.EventJournal.Storage.CompactionError != "" || h.EventJournal.Storage.Capacity == nil || h.EventJournal.Storage.Capacity.Tombstones.Rows != 1 || h.EventJournal.Storage.Capacity.Jobs.Rows != 0 {
		t.Fatal(h)
	}
	if _, err = e.Confirm(ConfirmRequest{Revision: state.Revision, RequestID: trial.RequestID}); err != nil {
		t.Fatal("recovered storage blocked confirmation", err)
	}
}

func TestBackgroundStorageMonitorCompactsCompletedWorkWithoutAnOperatorRequest(t *testing.T) {
	e, _ := fixture(t)
	j := attachJournal(t, e, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp, err := j.ProducerCheckpoint(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = j.ProducerTransition(ctx, "fixture", cp, cp.State, []events.Job{{Key: "done", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = j.ProducerComplete(ctx, "fixture", "done"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); e.monitorEventStore(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("storage monitor did not exit")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h := e.health()
		if c := h.EventJournal.Storage.Capacity; c != nil && c.Tombstones.Rows == 1 && c.Jobs.Rows == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("monitor did not reclaim completed work", e.health())
}
