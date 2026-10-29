package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestRegisteredHistoryCanDrainBeforeAnyCurrentWorkerObservation(t *testing.T) {
	e, _ := fixture(t)
	journal := attachJournal(t, e, 0)
	e.protocolRelays = newProtocolRelays(t, journal, e.store.Dir)
	if err := e.protocolRelays.Register(context.Background(), []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	// Control has restarted; neither a runtime nor a ready worker is needed
	// to deliver history from a queue registered before the restart.
	e.protocolRelays = newProtocolRelays(t, journal, e.store.Dir)
	before := e.State()
	batch := protocolevents.Batch{Resource: "wan", Events: []events.Event{protocolProbe("a")}}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/protocol-events", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	rows, err := journal.List(context.Background(), 0, 16)
	if err != nil || len(rows) != 1 || rows[0].Event.ID != batch.Events[0].ID {
		t.Fatal("restarted relay lost or duplicated history", rows, err)
	}
	if after := e.State(); after.Revision != before.Revision || after.ProducerEpoch != before.ProducerEpoch {
		t.Fatal("historical delivery changed current authority")
	}
	batch.Events = []events.Event{protocolProbe("b")}
	batch.Events[0].ID = "health/unknown/" + strings.Repeat("b", 32)
	// A valid-shaped foreign history entry cannot use another registration.
	if _, err := e.protocolRelays.Deliver(context.Background(), protocolevents.Batch{Resource: "wan", Events: batch.Events}); !errors.Is(err, protocolevents.ErrInvalidBatch) {
		t.Fatal("foreign history used an existing registration", err)
	}
}

func protocolProbe(id string) events.Event {
	return events.FromDaemon("health/wan/"+strings.Repeat(id, 32), daemonapi.DaemonEvent{Time: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), Type: "routerd.healthcheck.wan.passed", Daemon: daemonapi.DaemonRef{Name: old.DaemonKind + "-wan", Kind: old.DaemonKind, Instance: "wan"}, Resource: &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "HealthCheck", Name: "wan"}, Reason: "HealthCheckProbe", Severity: daemonapi.SeverityInfo})
}

func TestProtocolDeliveryFailureIsVisibleAndDoesNotClearAnotherProducer(t *testing.T) {
	e, _ := fixture(t)
	e.setProtocolErrorLocked("service.wan", errors.New("injected journal failure"))
	e.setProtocolErrorLocked("service.lan", errors.New("injected second failure"))
	e.setProtocolErrorLocked("service.wan", nil)
	if e.State().Healthy {
		t.Fatal("unrecovered producer was hidden")
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/health", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "protocol_events") || !strings.Contains(w.Body.String(), "injected second failure") {
		t.Fatal(w.Code, w.Body.String())
	}
	e.setProtocolErrorLocked("service.lan", nil)
	if !e.State().Healthy {
		t.Fatal("completed recovery left control unhealthy")
	}
}

func TestProtocolFailureBlocksConfirmationButNotRollbackRemovingItsProducer(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	r := request(e, "health-trial")
	r.Config.Resources = []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}}}
	r.ConfirmTimeoutSeconds = 60
	s, err := e.Commit(ctx, r)
	if err != nil || s.Confirmation == nil {
		t.Fatal(s, err)
	}
	e.setProtocolErrorLocked("service.wan", errors.New("injected protocol delivery failure"))
	if _, err = e.Confirm(ConfirmRequest{Revision: s.Revision, RequestID: r.RequestID}); err == nil {
		t.Fatal("unhealthy delivery was confirmed")
	}
	if err = e.Expire(ctx, s.Confirmation.Deadline.Add(time.Second)); err != nil {
		t.Fatal("delivery failure blocked rollback", err)
	}
	s = e.State()
	if s.Confirmation != nil || len(s.Config.Resources) != 0 || !s.Healthy || s.ProtocolEventError != "" {
		t.Fatal("removed producer's failure survived rollback", s)
	}
}

func TestProtocolBatchCapacityIsAtomicAndRejectsForeignEvents(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	journal := attachJournal(t, e, 1)
	relay := newProtocolRelays(t, journal, e.store.Dir)
	if err := relay.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	page := []events.Event{protocolProbe("a"), protocolProbe("b")}
	if _, err := relay.Deliver(ctx, protocolevents.Batch{Resource: "wan", Events: page}); err == nil {
		t.Fatal("capacity failure was acknowledged")
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 0 {
		t.Fatal("full batch partially published", rows, err)
	}
	if _, err = relay.Deliver(ctx, protocolevents.Batch{Resource: "wan", Events: page}); err == nil {
		t.Fatal("retry bypassed capacity")
	}
	rows, err = journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 0 {
		t.Fatal("retry partially published a rejected batch", rows, err)
	}
	ack, err := relay.Deliver(ctx, protocolevents.Batch{Resource: "wan", Events: page[:1]})
	if err != nil || len(ack.IDs) != 1 || ack.IDs[0] != page[0].ID {
		t.Fatal(ack, err)
	}
	foreign := protocolProbe("c")
	foreign.Topic = "configuration.committed"
	other, _ := fixture(t)
	roomy := attachJournal(t, other, 0)
	otherRelay := newProtocolRelays(t, roomy, other.store.Dir)
	if err := otherRelay.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]events.Event{nil, append(make([]events.Event, protocolevents.PageSize), foreign), {protocolProbe("c"), foreign}} {
		if _, err = otherRelay.Deliver(ctx, protocolevents.Batch{Resource: "wan", Events: invalid}); err == nil {
			t.Fatal("invalid batch accepted", invalid)
		}
	}
	// Entire page validation precedes any journal mutation.
	rows, err = roomy.List(ctx, 0, 16)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func newProtocolRelays(t *testing.T, journal *events.Store, root string) *protocolevents.Retirement {
	t.Helper()
	r, err := protocolevents.NewRetirement(context.Background(), journal, root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
