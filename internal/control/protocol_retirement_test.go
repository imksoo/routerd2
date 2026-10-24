package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/imksoo/routerd2/internal/protocolevents"
)

func TestRetirementPendingSurvivesSourceErrorPruningAndMakesSuccessfulACKUnhealthy(t *testing.T) {
	e, _ := fixture(t)
	e.setProtocolErrorLocked(protocolRetirementError, errors.New("retired queue pending recovery"))
	e.setProtocolErrorLocked("service.wan", errors.New("old active delivery error"))
	e.pruneProtocolErrorsLocked()
	if e.State().Healthy || len(e.protocolErrors) != 1 {
		t.Fatal("source removal hid retired work", e.State(), e.protocolErrors)
	}
	w := httptest.NewRecorder()
	e.writeConfigurationState(w, e.state, true)
	var ack map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ack) != nil || ack["healthy"] != false {
		t.Fatal("successful commit claimed recovered history", w.Code, w.Body.String())
	}
	e.setProtocolErrorLocked(protocolRetirementError, nil)
	if !e.State().Healthy {
		t.Fatal("finished recovery stayed unhealthy")
	}
}

func TestNoHealthSourceDoesNotAllocateARelayRegistry(t *testing.T) {
	e, _ := fixture(t)
	journal := attachJournal(t, e, 0)
	e.protocolRelays = protocolevents.NewRetirement(journal, e.store.Dir)
	if err := e.registerProtocolRelaysLocked(context.Background(), e.state.Config); err != nil {
		t.Fatal(err)
	}
	if _, found, err := journal.LookupProducer(context.Background(), "control-health-relays"); err != nil || found {
		t.Fatal("unused component allocated registry", found, err)
	}
}
