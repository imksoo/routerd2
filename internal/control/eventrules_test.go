package control

import (
	"context"
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
