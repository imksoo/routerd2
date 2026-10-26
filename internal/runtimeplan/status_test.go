package runtimeplan

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestServiceStatusRemainsBoundedAndProvidesDetailAndIssuePages(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.config.revision, r.phase = 7, "ready"
	large, _ := json.Marshal(map[string]any{"active": true, "routes": strings.Repeat("large route information ", 4096)})
	for i := 0; i < 128; i++ {
		name := fmt.Sprintf("service.instance-%d", i)
		r.bindings[name] = binding{instance: serviceconfig.Instance{Name: name, Role: "bgp"}, digest: strings.Repeat("f", 64)}
		r.records[name] = serviceconfig.Update{Name: name, Role: "bgp", Generation: 2, Sequence: 3, Snapshot: large}
	}
	// JSON HTML escaping must be counted too; a short raw string can grow six
	// times in the actual HTTP response. Aggregate status must stay bounded.
	r.records["service.instance-1"] = serviceconfig.Update{Snapshot: json.RawMessage(`{"detail":"` + strings.Repeat("<", 3000) + `"}`)}
	for i := 0; i < 1000; i++ {
		r.routeIssues = append(r.routeIssues, RouteIssue{Service: "instance", Prefix: fmt.Sprintf("2001:db8:%x::/64", i), Reason: strings.Repeat("<", 5000)})
	}
	r.lastError = strings.Repeat("<", 20000)
	h := r.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Body.Len() > localapi.MaxBody {
			t.Fatalf("GET %s exceeded private response bound: %d bytes", path, w.Body.Len())
		}
		return w
	}
	var state Status
	if err := json.Unmarshal(get("/v1/services").Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Instances) != 128 || state.RouteIssueCount != 1000 || len(state.RouteIssues) != 128 || !state.RouteIssues[0].ReasonTruncated {
		t.Fatal("bounded status hid its complete instance/issue counts", state.RouteIssueCount)
	}
	for _, instance := range state.Instances {
		if !instance.SnapshotTruncated || instance.SnapshotBytes < 4096 && instance.Name != "service.instance-1" || len(instance.Snapshot) != 0 {
			t.Fatal("large snapshot lacks a bounded-detail indicator", instance)
		}
		if instance.Name != "service.instance-1" && !instance.OwnershipExpired {
			t.Fatal("last observed active state hid expired forwarding ownership")
		}
	}
	var instance InstanceStatus
	if err := json.Unmarshal(get("/v1/services/service.instance-0").Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.SnapshotTruncated || string(instance.Snapshot) != string(large) {
		t.Fatal("detail endpoint failed to retain the complete snapshot")
	}
	var page RouteIssuePage
	if err := json.Unmarshal(get("/v1/services/route-issues?offset=128").Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1000 || page.Offset != 128 || page.NextOffset == nil || *page.NextOffset != 256 || page.Issues[0].Prefix != r.routeIssues[128].Prefix {
		t.Fatal("route issue pagination lost its continuation", page)
	}
	for _, path := range []string{"/v1/services/route-issues?offset=-1", "/v1/services/route-issues?offset=9999999999999999999999", "/v1/services/unknown"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code < 400 {
			t.Fatal("invalid observation request accepted", path)
		}
	}
	if len(r.routeIssues[0].Reason) != 5000 || r.routeIssues[0].ReasonTruncated || string(r.records["service.instance-0"].Snapshot) != string(large) {
		t.Fatal("observation mutated accepted facts")
	}
}
