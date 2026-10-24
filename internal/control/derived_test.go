package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestLiteralDerivedBootstrapPublishesWithoutInputHistoryAndSurvivesControlRecovery(t *testing.T) {
	ctx := context.Background()
	e, n := fixture(t)
	journal := attachJournal(t, e, 0)
	spec := api.DerivedEventSpec{Topic: "routerd.literal", Inputs: []api.ReadyWhenSpec{{Field: "literal", NotEmpty: true}}, EmitInitial: true}
	current := e.state.Committed
	current.Config = config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "derived/literal", DerivedEvent: &spec}}}
	if err := e.store.Save(current); err != nil {
		t.Fatal(err)
	}
	e = New(e.store, current, n)
	e.journal = journal
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessDerivedEvents(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 || rows[0].Event.Topic != "routerd.literal.asserted" {
		t.Fatal(rows, err)
	}
	response := callView(t, e.Handler(), http.MethodGet, "/v1/derived/derived%2Fliteral", nil)
	var view derivedcontroller.Observation
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil || response.Code != 200 || view.State.Asserted == nil || !*view.State.Asserted {
		t.Fatal("live current view", response.Code, view, err)
	}
	e = New(e.store, current, n)
	e.journal = journal
	if err = e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.ProcessDerivedEvents(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 {
		t.Fatal("initial output duplicated on control recovery", rows, err)
	}
}
