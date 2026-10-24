package control

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
)

func TestDurableObservationPagesAndStrictCursorQueries(t *testing.T) {
	e, _ := fixture(t)
	j := attachJournal(t, e, 0)
	for _, id := range []string{"page-one", "page-two"} {
		if _, err := e.Commit(context.Background(), request(e, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.FlushEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = j
	first := httptest.NewRecorder()
	e.Handler().ServeHTTP(first, httptest.NewRequest("GET", "/v1/observations?after_cursor=0&limit=1", nil))
	var page struct {
		Records    []events.Record `json:"records"`
		NextCursor uint64          `json:"next_cursor"`
		More       bool            `json:"more"`
	}
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Records) != 1 || !page.More || page.NextCursor != 1 {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	e.Handler().ServeHTTP(second, httptest.NewRequest("GET", "/v1/observations?after_cursor=1&limit=1", nil))
	if second.Code != 200 || json.Unmarshal(second.Body.Bytes(), &page) != nil || len(page.Records) != 1 || page.More || page.NextCursor != 2 {
		t.Fatalf("second page: %d %s", second.Code, second.Body.String())
	}
	for _, query := range []string{"", "?after_cursor=-1", "?after_cursor=18446744073709551615", "?after_cursor=0&limit=17", "?after_cursor=0&limit=0", "?after_cursor=0&after_cursor=1", "?after_cursor=0&other=1", "?after_cursor=0&limit=1&limit=2"} {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/observations"+query, nil))
		if w.Code != 400 {
			t.Fatalf("query %q: %d", query, w.Code)
		}
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/observations?after_cursor=99", nil))
	if w.Code != 409 {
		t.Fatalf("future cursor: %d", w.Code)
	}
}
