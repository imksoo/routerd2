package control

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventStoreWarningIsVisibleBeforeFullAndDoesNotDegradeUsableControl(t *testing.T) {
	e, _ := fixture(t)
	j := attachJournal(t, e, 5)
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := j.Append(context.Background(), protocolProbe(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.RefreshEventStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := e.health()
	if !h.Healthy || h.EventJournal.Storage.Capacity == nil || h.EventJournal.Storage.Capacity.Pressure != "warning" || h.EventJournal.Storage.Capacity.Events.Rows != 4 {
		t.Fatal(h)
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/event-store", nil))
	var status EventStoreHealth
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Capacity == nil || status.Capacity.Events.RowLimit != 5 {
		t.Fatal(w.Code, w.Body.String())
	}
	bad := httptest.NewRecorder()
	e.Handler().ServeHTTP(bad, httptest.NewRequest("GET", "/v1/event-store?unknown=1", nil))
	if bad.Code != 400 {
		t.Fatal(bad.Code)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if cached := e.health(); !cached.Healthy || cached.EventJournal.Storage.Capacity == nil || cached.EventJournal.Storage.Capacity.Events.Rows != 4 {
		t.Fatal("health attempted storage IO instead of returning its last bounded observation", cached)
	}
	if err := e.RefreshEventStore(context.Background()); err == nil || e.State().Healthy || e.health().EventJournal.Storage.LastError == "" {
		t.Fatal("failed monitoring hid unavailable storage")
	}
}

func TestFreshEventCapacityBlocksConfirmationBeforeAWriteFailure(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	j := attachJournal(t, e, 2)
	r := request(e, "capacity-trial")
	r.ConfirmTimeoutSeconds = 60
	s, err := e.Commit(ctx, r)
	if err != nil || s.Confirmation == nil {
		t.Fatal(s, err)
	}
	if err = e.FlushEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Append(ctx, protocolProbe("e")); err != nil {
		t.Fatal(err)
	}
	// No periodic monitor has run: confirmation itself must observe full
	// capacity rather than trusting a stale healthy cache.
	if _, err = e.Confirm(ConfirmRequest{Revision: s.Revision, RequestID: r.RequestID}); err == nil {
		t.Fatal("full unobserved journal was confirmed")
	}
	if e.State().Confirmation == nil || !strings.Contains(e.State().EventStoreError, "events_rows") {
		t.Fatal(e.State())
	}
	if retry, err := j.Append(ctx, protocolProbe("e")); err != nil || retry.Cursor == 0 {
		t.Fatal("alarm broke stable-ID ACK", retry, err)
	}
	if err = e.Expire(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if restored := e.State(); restored.Confirmation != nil || restored.Revision != s.Revision+1 || len(restored.EventOutbox) != 1 || restored.EventStoreError == "" {
		t.Fatal("full journal blocked rollback or lost its durable pending observation", restored)
	}
}
