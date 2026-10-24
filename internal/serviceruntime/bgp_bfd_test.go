package serviceruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bgp"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

type bfdRoundTripFunc func(*http.Request) (*http.Response, error)

func (f bfdRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBFDWatcherFencesRecipientAndRetainsCoalescedFailure(t *testing.T) {
	digest := strings.Repeat("a", 64)
	var requests atomic.Int32
	second := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &effectsClient{client: &localapi.Client{HTTP: &http.Client{Transport: bfdRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/v1/services/bgp-core/bfd" || r.URL.Query().Get("config_digest") != digest || r.URL.Query().Get("generation") != "9" {
			t.Errorf("unfenced BFD request: %s", r.URL)
		}
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(`{"error":"recipient has not published status"}`))}, nil
		}
		if requests.Load() == 2 {
			close(second)
		}
		b, _ := json.Marshal(bgpconfig.BFDProjection{ConfigDigest: digest, Generation: 9})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}}}
	updates, done := make(chan bgp.BFDUpdate, 1), make(chan struct{})
	go func() { defer close(done); client.watchBFD(ctx, "bgp-core", digest, 9, updates) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("watcher did not recover from initial generation conflict")
	}
	var update bgp.BFDUpdate
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !update.Available {
		select {
		case update = <-updates:
		case <-deadline.C:
			t.Fatal("success did not replace unavailable coalesced value")
		}
	}
	if update.Failures != 1 || update.Received.BootID == "" || update.Received.BoottimeNanoseconds <= 0 || update.Projection.Generation != 9 {
		t.Fatalf("watcher lost failure/finite receipt evidence: %+v", update)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not cancel")
	}
}

func TestBFDWatcherBoundsPendingRequestAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	client := &effectsClient{client: &localapi.Client{HTTP: &http.Client{Transport: bfdRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 201*time.Millisecond {
			t.Error("BFD projection request has no short deadline")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.watchBFD(ctx, "bgp-core", strings.Repeat("a", 64), 9, make(chan bgp.BFDUpdate, 1))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watcher did not request projection")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight request prevented cancellation")
	}
}
