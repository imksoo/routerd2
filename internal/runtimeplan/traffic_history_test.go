// SPDX-License-Identifier: BSD-3-Clause
package runtimeplan

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestTrafficHistoryReadsOwnedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flows.db")
	cfg, err := json.Marshal(trafficflowconfig.Config{HistoryPath: path})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{bindings: map[string]binding{"service.flows": {instance: serviceconfig.Instance{Name: "service.flows", Role: "traffic_flow", Config: cfg}}}}
	mux := http.NewServeMux()
	runtime.trafficHistoryRoute(mux)
	request := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/services/service.flows/traffic-history"+query, nil))
		return response
	}
	if response := request(""); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a read created the collector database", err)
	}
	db, err := logstore.OpenTrafficFlowLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := time.Now().UTC().Add(-time.Minute)
	rows := []logstore.TrafficFlow{
		{FlowKey: "old", StartedAt: started, ClientAddress: "2001:db8::2", PeerAddress: "2001:db8::3", Protocol: "tcp", Accounting: true, BytesOut: 17, TLSSNI: "example.test"},
		{FlowKey: "new", StartedAt: started.Add(time.Second), ClientAddress: "2001:db8::2", PeerAddress: "2001:db8::3", Protocol: "udp", Accounting: true, BytesOut: 19},
	}
	for _, row := range rows {
		if err := db.UpsertActive(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	var page TrafficHistory
	response := request("?limit=1")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if !page.Truncated || len(page.Flows) != 1 || page.Flows[0].FlowKey != "new" {
		t.Fatal("newest bounded result", page)
	}
	query := url.Values{"client": {"2001:0db8:0000::2"}, "protocol": {"TCP"}, "since": {started.Format(time.RFC3339Nano)}, "until": {started.Format(time.RFC3339Nano)}}
	response = request("?" + query.Encode())
	page = TrafficHistory{}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if page.Truncated || len(page.Flows) != 1 || page.Flows[0].TLSSNI != "example.test" || page.Flows[0].BytesOut != 17 {
		t.Fatal("filter or retained metadata", page)
	}
	if err := db.EndMissing(t.Context(), []string{"new"}, started.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	response = request("?protocol=tcp")
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Flows) != 1 || page.Flows[0].EndedAt.IsZero() {
		t.Fatal("ended history disappeared", page)
	}
	if response := request("?until=not-a-time"); response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
	if response := request("?limit=1001"); response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
}
