package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestCommittedEventRuleRunsAndResumesThroughControlEngine(t *testing.T) {
	ctx := context.Background()
	e, n := fixture(t)
	journal := attachJournal(t, e, 0)
	r := request(e, "enable-rule")
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "configuration.committed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.commits"}}
	r.Config.Resources = append(r.Config.Resources, resourceprogram.Resource{ID: "commit-rule", EventRule: &spec})
	if _, err := e.Commit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessEventRules(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := e.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e = New(e.store, current, n)
	e.journal = journal
	if err = e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r = CommitRequest{ExpectedRevision: current.Revision, RequestID: "same-rule", Config: current.Config}
	if _, err = e.Commit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.ProcessEventRules(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	outputs := 0
	for _, row := range rows {
		if row.Event.Topic == "routerd.commits" {
			outputs++
		}
	}
	if outputs != 1 {
		t.Fatal("live engine lost/resampled rule state", rows)
	}
	if h := e.health(); !h.Healthy || h.EventRules.LastError != "" || len(h.EventRules.Rules) != 1 {
		t.Fatal(h)
	}
}

func TestLifecycleSaveFailureKeepsCommittedSourceButReturnsUnhealthyAcknowledgment(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	attachJournal(t, e, 0)
	injector, err := sql.Open("sqlite", filepath.Join(e.store.Dir, "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_rule_registry BEFORE UPDATE ON producers BEGIN SELECT RAISE(ABORT,'injected lifecycle persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "configuration.committed", Threshold: 1}, Emit: api.EventRuleEmitSpec{Topic: "routerd.commits"}}
	r := request(e, "persisted-but-rule-pending")
	r.ConfirmTimeoutSeconds = 5
	r.Config.Resources = append(r.Config.Resources, resourceprogram.Resource{ID: "rule", EventRule: &spec})
	response := callView(t, e.Handler(), http.MethodPost, "/v1/config/commit", r)
	var ack State
	if err = json.Unmarshal(response.Body.Bytes(), &ack); err != nil || response.Code != http.StatusOK || ack.Revision != 2 || ack.Healthy || ack.EventRuleError == "" {
		t.Fatalf("committed lifecycle failure was hidden or reported as rollback: %d %+v %v", response.Code, ack, err)
	}
	stored, err := e.store.Load()
	if err != nil || stored.Revision != 2 || len(stored.Config.Resources) != 1 {
		t.Fatal("committed source lost", stored, err)
	}
	if h := e.health(); h.Healthy || h.EventRules.LastError == "" {
		t.Fatal(h)
	}
	if _, err = e.Confirm(ConfirmRequest{Revision: 2, RequestID: r.RequestID}); err == nil {
		t.Fatal("confirmed degraded event controller")
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_rule_registry`); err != nil {
		t.Fatal(err)
	}
	if err = e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.ProcessEventRules(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if h := e.health(); !h.Healthy || h.EventRules.LastError != "" {
		t.Fatal("controller did not recover", h)
	}
}
