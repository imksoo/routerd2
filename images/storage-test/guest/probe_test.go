package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

func TestStorageWitnessWaitsForEventAndProducerAcknowledgement(t *testing.T) {
	s := control.State{Committed: config.Committed{Revision: 2, EventOutbox: []config.ChangeEvent{{ID: "pending"}}}, Healthy: true}
	p := control.ObservationPage{Records: []events.Record{{Cursor: 1, Event: events.Event{ID: "persisted", Topic: "configuration.committed", Source: "control", ObservedAt: time.Now().UTC(), Attributes: map[string]string{"revision": "2"}}}}}
	if _, ok := settledConfigEvent(s, p); ok {
		t.Fatal("raw config hash sampled before producer ACK removed pending metadata")
	}
	s.EventOutbox = nil
	if _, ok := settledConfigEvent(s, control.ObservationPage{}); ok {
		t.Fatal("empty producer outbox substituted for durable publication evidence")
	}
	if r, ok := settledConfigEvent(s, p); !ok || r.Event.ID != "persisted" {
		t.Fatal("settled publication not witnessed", r, ok)
	}
	p.Records[0].Event.Attributes["revision"] = "1"
	if _, ok := settledConfigEvent(s, p); ok {
		t.Fatal("an older event satisfied current configuration acceptance")
	}
}

func TestStoragePostCommitWaitRequiresHealthyProjectionAndSettledPublication(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/config" {
			n := reads.Add(1)
			s := control.State{Committed: config.Committed{Revision: 2}, Healthy: n > 1, Services: &runtimeplan.Status{Revision: 2, Phase: "ready"}}
			if n == 1 {
				s.Services.Phase = "applying"
				s.Services.Revision = 1
			}
			if n < 3 {
				s.EventOutbox = []config.ChangeEvent{{ID: "pending"}}
			}
			_ = json.NewEncoder(w).Encode(s)
			return
		}
		_ = json.NewEncoder(w).Encode(control.ObservationPage{Records: []events.Record{{Cursor: 1, Event: events.Event{ID: "stable", Topic: "configuration.committed", Source: "control", ObservedAt: time.Now().UTC(), Attributes: map[string]string{"revision": "2"}}}}})
	}))
	defer server.Close()
	client := &localapi.Client{HTTP: &http.Client{Transport: rewriteFixtureURL{target: server.URL}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The former one-shot check aborts on this legal intermediate state,
	// before its existing publication/ACK settling loop can observe recovery.
	if _, err := readHealthyConfigFrom(ctx, client); err == nil {
		t.Fatal("one-shot fixture did not reproduce an unhealthy intermediate state")
	} else {
		t.Logf("one-shot health refusal reproduced: %v", err)
	}
	s, event, err := awaitPublishedConfigEventFrom(ctx, client)
	if err != nil || reads.Load() < 3 || !s.Healthy || len(s.EventOutbox) != 0 || event.Event.ID != "stable" {
		t.Fatal("accepted unsettled post-commit witness", s, event, err)
	}
}

func TestStorageUnhealthyPostCommitStopsAtDeadlineWithDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(control.State{Committed: config.Committed{Revision: 2}, Services: &runtimeplan.Status{Revision: 1, Phase: "applying"}})
	}))
	defer server.Close()
	client := &localapi.Client{HTTP: &http.Client{Transport: rewriteFixtureURL{target: server.URL}}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _, err := awaitPublishedConfigEventFrom(ctx, client)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "revision=2 healthy=false services=applying service_revision=1") {
		t.Fatal("lost health diagnostic or deadline", err)
	}
}

type rewriteFixtureURL struct{ target string }

func (r rewriteFixtureURL) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	u := *copy.URL
	copy.URL = &u
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(r.target, "http://")
	return http.DefaultTransport.RoundTrip(copy)
}
