// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/imksoo/routerd/pkg/api"
	resolvercfg "github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd/pkg/logstore"
)

func TestNormalizeDaemonExit(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	runtimeErr := errors.New("listen failed")

	tests := []struct {
		name    string
		ctx     context.Context
		err     error
		wantErr error
	}{
		{name: "canceled context", ctx: canceledCtx, err: context.Canceled},
		{name: "wrapped canceled context", ctx: canceledCtx, err: fmt.Errorf("run: %w", context.Canceled)},
		{name: "runtime error after cancellation", ctx: canceledCtx, err: runtimeErr, wantErr: runtimeErr},
		{name: "unexpected cancellation", ctx: context.Background(), err: context.Canceled, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeDaemonExit(tt.ctx, tt.err)
			if !errors.Is(got, tt.wantErr) || (got == nil) != (tt.wantErr == nil) {
				t.Fatalf("normalizeDaemonExit() error = %v, want %v", got, tt.wantErr)
			}
		})
	}
}

func TestCacheRejectsResponseFromPreviousGeneration(t *testing.T) {
	d := &daemon{cache: map[string]cacheEntry{}, generation: 1}
	d.cacheSet("answer", []byte("old"), time.Hour, 10, 1)
	d.stateMu.Lock()
	d.generation = 2
	d.stateMu.Unlock()
	d.mu.Lock()
	clear(d.cache)
	d.mu.Unlock()

	// This models an upstream response which started before reload and
	// completed after the new runtime was installed.
	d.cacheSet("answer", []byte("late-old"), time.Hour, 10, 1)
	if _, ok := d.cacheGet("answer", 2); ok {
		t.Fatal("response from previous runtime generation entered current cache")
	}
}

func assertZoneARecord(t *testing.T, zones *zoneTable, name, wantIP string) {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypeA)
	resp, ok := zones.Answer(req, []string{"DNSZone/lan-zone"})
	if !ok || len(resp.Answer) != 1 {
		t.Fatalf("A record missing for %s: ok=%v resp=%v", name, ok, resp)
	}
	if a, isA := resp.Answer[0].(*dns.A); !isA || a.A.String() != wantIP {
		t.Fatalf("A record for %s = %v, want %s", name, resp.Answer[0], wantIP)
	}
}

func assertZonePTRRecord(t *testing.T, zones *zoneTable, name, wantTarget string) {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypePTR)
	resp, ok := zones.Answer(req, []string{"DNSZone/lan-zone"})
	if !ok || len(resp.Answer) != 1 {
		t.Fatalf("PTR record missing for %s: ok=%v resp=%v", name, ok, resp)
	}
	if ptr, isPTR := resp.Answer[0].(*dns.PTR); !isPTR || ptr.Ptr != wantTarget {
		t.Fatalf("PTR record for %s = %v, want %s", name, resp.Answer[0], wantTarget)
	}
}

func assertZoneNoPTRRecord(t *testing.T, zones *zoneTable, name string) {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypePTR)
	if resp, ok := zones.Answer(req, []string{"DNSZone/lan-zone"}); ok {
		t.Fatalf("unexpected PTR record for %s: %v", name, resp)
	}
}

func TestObservedStatusIncludesQueryLogStats(t *testing.T) {
	config := testResolverConfig(t, []int{5053})
	d := newTestDaemon(t, config)
	queryLog, err := logstore.OpenDNSQueryLog(filepath.Join(t.TempDir(), "dns-queries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer queryLog.Close()
	d.queryLog = queryLog
	now := time.Date(2026, 7, 8, 1, 2, 3, 0, time.UTC)
	if err := queryLog.Record(context.Background(), logstore.DNSQuery{
		Timestamp:     now,
		ClientAddress: "192.0.2.10",
		QuestionName:  "observed.example.",
		QuestionType:  "A",
		ResponseCode:  "NOERROR",
	}); err != nil {
		t.Fatal(err)
	}

	observed := d.observedStatus()
	if observed["queryLogEnabled"] != "true" {
		t.Fatalf("queryLogEnabled = %q", observed["queryLogEnabled"])
	}
	if observed["queryLogRecords"] != "1" || observed["queryLogRecordErrors"] != "0" {
		t.Fatalf("query log counts = %#v", observed)
	}
	if observed["queryLogDBBytes"] == "" || observed["queryLogWALBytes"] == "" {
		t.Fatalf("query log sizes missing: %#v", observed)
	}
	if observed["queryLogLastRecordAt"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("queryLogLastRecordAt = %q", observed["queryLogLastRecordAt"])
	}
}

func TestRecordQueryReopensClosedQueryLog(t *testing.T) {
	config := testResolverConfig(t, []int{5053})
	d := newTestDaemon(t, config)
	path := filepath.Join(t.TempDir(), "dns-queries.db")
	queryLog, err := logstore.OpenDNSQueryLog(path)
	if err != nil {
		t.Fatal(err)
	}
	d.queryLog = queryLog
	if err := queryLog.Close(); err != nil {
		t.Fatal(err)
	}

	req := new(dns.Msg)
	req.SetQuestion("reopen.example.", dns.TypeA)
	resp := new(dns.Msg)
	resp.SetReply(req)
	d.recordQuery("192.0.2.10:12345", req, resolveResult{Response: resp, ResponseCode: dns.RcodeToString[resp.Rcode]}, time.Millisecond)

	d.queryLogMu.Lock()
	reopened := d.queryLog
	d.queryLogMu.Unlock()
	defer d.closeQueryLog()
	if reopened == nil || reopened == queryLog {
		t.Fatal("query log was not reopened")
	}
	rows, err := reopened.List(context.Background(), logstore.DNSQueryFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].QuestionName != "reopen.example" {
		t.Fatalf("unexpected query rows: %#v", rows)
	}
}

func testResolverConfig(t *testing.T, ports []int) resolvercfg.RuntimeConfig {
	listen := make([]api.DNSResolverListenSpec, 0, len(ports))
	for _, port := range ports {
		listen = append(listen, api.DNSResolverListenSpec{
			Name:      fmt.Sprintf("listen-%d", port),
			Addresses: []string{"127.0.0.1"},
			Port:      port,
		})
	}
	return resolvercfg.RuntimeConfig{
		Resource: "lab",
		Spec: api.DNSResolverSpec{
			Listen: listen,
			Sources: []api.DNSResolverSourceSpec{{
				Name:    "zones",
				Kind:    "zone",
				Match:   []string{"lab.example"},
				ZoneRef: []string{"DNSZone/lan-zone"},
			}},
		},
		Zones: []resolvercfg.RuntimeZone{{
			Name: "lan-zone",
			Spec: api.DNSZoneSpec{
				Zone:        "lab.example",
				DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"dhcpv4"}},
				Records: []api.DNSZoneRecordSpec{{
					Hostname: "router",
					IPv4:     "192.0.2.1",
				}},
			},
		}},
	}
}

func testForwardConfig(t *testing.T, name, upstream string) resolvercfg.RuntimeConfig {
	return resolvercfg.RuntimeConfig{
		Resource: "lab",
		Spec: api.DNSResolverSpec{
			Listen: []api.DNSResolverListenSpec{{Name: "loopback", Addresses: []string{"127.0.0.1"}, Port: freeTCPPort(t)}},
			Sources: []api.DNSResolverSourceSpec{{
				Name:      name,
				Kind:      "forward",
				Match:     []string{"."},
				Upstreams: []string{upstream},
			}},
		},
	}
}

func newTestDaemon(t *testing.T, config resolvercfg.RuntimeConfig) *daemon {
	t.Helper()
	d, err := newDaemon(options{
		resource: "lab",

		stateFile: filepath.Join(t.TempDir(), "state.json"),
		eventFile: filepath.Join(t.TempDir(), "events.jsonl"),
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.shutdownDNSServers()
		d.closeQueryLog()
	})
	return d
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var out int
	if _, err := fmt.Sscanf(port, "%d", &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertDNSAnswer(t *testing.T, addr, qname, want string) {
	t.Helper()
	var lastErr error
	for i := 0; i < 20; i++ {
		req := new(dns.Msg)
		req.SetQuestion(qname, dns.TypeA)
		resp, _, err := (&dns.Client{Net: "udp", Timeout: 100 * time.Millisecond}).Exchange(req, addr)
		if err == nil && resp != nil && len(resp.Answer) == 1 {
			if a, ok := resp.Answer[0].(*dns.A); ok && a.A.String() == want {
				return
			}
			t.Fatalf("answer = %v, want %s", resp.Answer, want)
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("DNS query %s via %s failed: %v", qname, addr, lastErr)
}

func assertDNSNoResponse(t *testing.T, addr, qname string) {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(qname, dns.TypeA)
	resp, _, err := (&dns.Client{Net: "udp", Timeout: 100 * time.Millisecond}).Exchange(req, addr)
	if err == nil && resp != nil {
		t.Fatalf("unexpected DNS response via removed listener: %v", resp)
	}
}

func skipIfListenNotPermitted(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
		t.Skipf("network listen is not permitted in this environment: %v", err)
	}
}
