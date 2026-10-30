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
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestFirewallHistoryReadsCollectorDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	cfg, err := json.Marshal(firewalllogconfig.Config{Group: 1, HistoryPath: path})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{bindings: map[string]binding{"service.firewall": {instance: serviceconfig.Instance{Name: "service.firewall", Role: "firewall_log", Config: cfg}}}}
	mux := http.NewServeMux()
	runtime.firewallHistoryRoute(mux)
	request := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/services/service.firewall/firewall-history?"+values.Encode(), nil))
		return response
	}
	if response := request(nil); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created collector database", err)
	}
	db, err := logstore.OpenFirewallLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().UTC().Add(-time.Minute)
	if err := db.RecordBatch(t.Context(), []logstore.FirewallLogEntry{
		{Timestamp: at, SrcAddress: "2001:db8::2", DstAddress: "2001:db8::1", Action: "drop", RuleName: "block", Protocol: "tcp", DstPort: 443, TCPFlags: "SYN", InIface: "lan0", PacketBytes: 60},
		{Timestamp: at.Add(time.Second), SrcAddress: "192.0.2.2", DstAddress: "192.0.2.1", Action: "reject", RuleName: "refuse", Protocol: "udp", DstPort: 53},
	}); err != nil {
		t.Fatal(err)
	}
	var page FirewallHistory
	response := request(url.Values{"limit": {"1"}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if !page.AtLimit || len(page.Entries) != 1 || page.Entries[0].Action != "reject" {
		t.Fatal("bounded newest result", page)
	}
	response = request(url.Values{"source": {"2001:0db8:0000::2"}, "action": {"DROP"}, "since": {at.Format(time.RFC3339Nano)}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if page.AtLimit || len(page.Entries) != 1 || page.Entries[0].TCPFlags != "SYN" || page.Entries[0].RuleName != "block" || page.Entries[0].InIface != "lan0" {
		t.Fatal("exact filters and original packet fields", page)
	}
	response = request(url.Values{"since": {at.Add(time.Second).Format(time.RFC3339Nano)}, "action": {"drop"}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Entries) != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, query := range []url.Values{{"source": {"invalid"}}, {"since": {"invalid"}}, {"until": {at.Format(time.RFC3339Nano)}}} {
		if response := request(query); response.Code != http.StatusBadRequest {
			t.Fatal(query, response.Code, response.Body.String())
		}
	}
}
