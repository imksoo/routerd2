//go:build linux

package control

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
)

func TestEventFormatAPIUsesActualOwnedDatabaseWithoutHealthCache(t *testing.T) {
	e, _ := fixture(t)
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := call("/v1/event-store/format"); w.Code != 503 {
		t.Fatal("unavailable journal invented metadata", w.Code)
	}
	j := attachJournal(t, e, 10)
	want, err := j.InspectFormat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := call("/v1/event-store/format")
	var got events.FormatInspection
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got != want || w.Body.Len() > 1024 {
		t.Fatal("actual event format metadata", w.Code, w.Body.String())
	}
	if w := call("/v1/event-store/format?path=/other/store"); w.Code != 400 {
		t.Fatal("format API accepted caller-selected path", w.Code)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if w := call("/v1/event-store/format"); w.Code != 503 {
		t.Fatal("closed database returned cached success", w.Code)
	}
}
