package control

import (
	"context"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
)

func TestStartupProvenanceSurvivesControlCommitAndRestartRollback(t *testing.T) {
	e, n := fixture(t)
	ref, err := e.store.Snapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	selected := configref.Selection{Generation: 5, ExpectedRevision: 1, Snapshot: ref}
	c, err := e.store.AdoptStartup(selected)
	if err != nil {
		t.Fatal(err)
	}
	e = New(e.store, c, n)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := request(e, "later-unconfirmed")
	r.ConfirmTimeoutSeconds = 60
	if _, err := e.Commit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	store, current, err := config.OpenSelected(e.store.Dir, "/missing", &selected)
	if err != nil {
		t.Fatal(err)
	}
	e = New(store, current, n)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.State()
	if s.Revision != 4 || s.Confirmation != nil || s.Startup == nil || s.Startup.Selection != selected || s.Startup.Revision != 2 {
		t.Fatal("restart lost adopted origin or replayed the snapshot")
	}
	metadata, err := e.metadataForState(s)
	if err != nil || metadata.Startup == nil || *metadata.Startup != *s.Startup {
		t.Fatal("bounded metadata omitted adopted startup", err)
	}
}
