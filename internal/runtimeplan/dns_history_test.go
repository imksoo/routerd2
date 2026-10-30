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

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestDNSHistoryReadsResolverDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queries.db")
	cfg, err := json.Marshal(dnsconfig.Config{Runtime: dnsconfig.RuntimeConfig{Spec: api.DNSResolverSpec{QueryLog: api.DNSResolverQueryLogSpec{Enabled: true, Path: path}}}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{bindings: map[string]binding{"service.resolver": {instance: serviceconfig.Instance{Name: "service.resolver", Role: "dns", Config: cfg}}}}
	mux := http.NewServeMux()
	runtime.dnsHistoryRoute(mux)
	request := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/services/service.resolver/dns-history?"+values.Encode(), nil))
		return response
	}
	if response := request(nil); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created resolver database", err)
	}
	db, err := logstore.OpenDNSQueryLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().UTC().Add(-time.Minute)
	for _, row := range []logstore.DNSQuery{
		{Timestamp: at, ClientAddress: "2001:db8::2", QuestionName: "Example.Test.", QuestionType: "AAAA", ResponseCode: "NOERROR", Answers: []string{"2001:db8::9"}, Upstream: "[2001:db8::53]:53", Duration: 23 * time.Millisecond},
		{Timestamp: at.Add(time.Second), ClientAddress: "192.0.2.2", QuestionName: "missing.test.", QuestionType: "A", ResponseCode: "NXDOMAIN"},
	} {
		if err := db.Record(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	var page DNSHistory
	response := request(url.Values{"limit": {"1"}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if !page.Truncated || len(page.Queries) != 1 || page.Queries[0].ResponseCode != "NXDOMAIN" {
		t.Fatal("bounded newest result", page)
	}
	response = request(url.Values{"client": {"2001:0db8:0000::2"}, "name": {"example.test."}, "response": {"noerror"}, "since": {at.Format(time.RFC3339Nano)}, "until": {at.Format(time.RFC3339Nano)}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if page.Truncated || len(page.Queries) != 1 || len(page.Queries[0].Answers) != 1 || page.Queries[0].Duration != 23*time.Millisecond {
		t.Fatal("exact filters and retained answer", page)
	}
	response = request(url.Values{"response": {"NXDOMAIN"}, "name": {"%.test"}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Queries) != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
}
