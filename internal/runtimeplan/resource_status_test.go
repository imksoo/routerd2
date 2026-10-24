package runtimeplan

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func TestResourceObservationPagesRejectMixedSnapshots(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{})
	r.revision, r.projectionGeneration, r.phase = 7, 12, "ready"
	r.resourceStates = map[string]resourceprogram.ResourceState{}
	for i := 2999; i >= 0; i-- {
		r.resourceStates[fmt.Sprintf("resource-%04d", i)] = resourceprogram.ResourceState{When: resourceprogram.Indeterminate}
	}
	h := r.Handler()
	get := func(path string, code int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != code || w.Body.Len() > localapi.MaxBody {
			t.Fatalf("resource page: code=%d length=%d body=%s", w.Code, w.Body.Len(), w.Body.String())
		}
		return w.Body.Bytes()
	}
	var page struct {
		Revision, ProjectionGeneration uint64
		ObservationID                  string `json:"observation_id"`
		Total, Offset                  int
		NextOffset                     *int `json:"next_offset"`
		Resources                      []struct {
			ID    string
			State resourceprogram.ResourceState
		}
	}
	if err := json.Unmarshal(get("/v1/services/resources", 200), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 3000 || page.NextOffset == nil || *page.NextOffset != 128 || len(page.Resources) != 128 || page.Resources[0].ID != "resource-0000" || len(page.ObservationID) != 64 {
		t.Fatalf("invalid bounded page: %+v", page)
	}
	continuation := fmt.Sprintf("/v1/services/resources?offset=128&observation_id=%s", page.ObservationID)
	if err := json.Unmarshal(get(continuation, 200), &page); err != nil || page.Resources[0].ID != "resource-0128" {
		t.Fatal("lost resource continuation", err)
	}
	// Readiness can change without a network generation change. Pin the full
	// observation, rather than only operator revision/projection generation.
	r.resourceStates["resource-2999"] = resourceprogram.ResourceState{Active: true, Ready: resourceprogram.True, When: resourceprogram.True}
	get(continuation, 409)
	for _, path := range []string{"?offset=-1", "?offset=128", "?offset=0&offset=1", "?unknown=x", "?observation_id=bad"} {
		get("/v1/services/resources"+path, 400)
	}
	if len(r.resourceStates) != 3000 || r.revision != 7 || r.projectionGeneration != 12 {
		t.Fatal("status read mutated source or projection")
	}
}
