package control

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/protocolevents"
)

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

func TestProtocolBatchRetriesPartialStorageWithoutDuplicatingOrAcceptingForeignEvents(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	journal := attachJournal(t, e, 1)
	page := []events.Event{protocolProbe("a"), protocolProbe("b")}
	if _, err := appendProtocolBatch(ctx, journal, "wan", page); err == nil {
		t.Fatal("capacity failure was acknowledged")
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 || rows[0].Event.ID != page[0].ID {
		t.Fatal(rows, err)
	}
	if _, err = appendProtocolBatch(ctx, journal, "wan", page); err == nil {
		t.Fatal("retry bypassed capacity")
	}
	rows, err = journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 {
		t.Fatal("retry duplicated saved prefix", rows, err)
	}
	ack, err := appendProtocolBatch(ctx, journal, "wan", page[:1])
	if err != nil || len(ack.IDs) != 1 || ack.IDs[0] != page[0].ID {
		t.Fatal(ack, err)
	}
	foreign := protocolProbe("c")
	foreign.Topic = "configuration.committed"
	other, _ := fixture(t)
	roomy := attachJournal(t, other, 0)
	for _, invalid := range [][]events.Event{nil, append(make([]events.Event, protocolevents.PageSize), foreign), {protocolProbe("c"), foreign}} {
		if _, err = appendProtocolBatch(ctx, roomy, "wan", invalid); err == nil {
			t.Fatal("invalid batch accepted", invalid)
		}
	}
	// Entire page validation precedes any journal mutation.
	rows, err = roomy.List(ctx, 0, 16)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
