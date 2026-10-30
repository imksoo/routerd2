// SPDX-License-Identifier: BSD-3-Clause
package trafficflow

import (
	"fmt"
	"github.com/florianl/go-conntrack"
	"github.com/imksoo/routerd/pkg/dpi"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
)

// The former NOT IN placeholders exceeded SQLite's 32766 variable limit.
func TestCompleteSampleBeyondSQLiteParameterLimit(t *testing.T) {
	ctx := t.Context()
	db, err := openHistory(ctx, filepath.Join(t.TempDir(), "flows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	flows := make([]logstore.TrafficFlow, 32766)
	keys := make([]string, len(flows))
	for i := range flows {
		keys[i] = fmt.Sprintf("flow-%d", i)
		flows[i] = logstore.TrafficFlow{FlowKey: keys[i], StartedAt: now, Protocol: "udp", BytesOut: 17}
	}
	started := time.Now()
	if err := syncActive(ctx, db, flows, keys, now); err != nil {
		t.Fatal(err)
	}
	var count, ended int
	if err := db.QueryRowContext(ctx, "SELECT count(*),count(ts_ended) FROM flows").Scan(&count, &ended); err != nil {
		t.Fatal(err)
	}
	if count != len(flows) || ended != 0 {
		t.Fatal("complete sample lost active connections", count, ended)
	}
	// A later complete sample ends only the missing tuple and keeps the first
	// observation time when cumulative counters change.
	flows = flows[1:]
	keys = keys[1:]
	flows[0].BytesOut = 31
	flows[0].StartedAt = now.Add(time.Second)
	if err := syncActive(ctx, db, flows, keys, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var bytes, stamp int64
	if err := db.QueryRowContext(ctx, "SELECT count(ts_ended) FROM flows").Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT bytes_out,ts_started FROM flows WHERE flow_key='flow-1'").Scan(&bytes, &stamp); err != nil {
		t.Fatal(err)
	}
	if ended != 1 || bytes != 31 || stamp != now.UnixNano() {
		t.Fatal("sample identity/end semantics changed", ended, bytes, stamp)
	}
	t.Logf("two complete samples across the SQLite parameter boundary committed in %s", time.Since(started))
}

// The same tuple can disappear and be replaced between two complete samples.
func TestReusedTupleKeepsSeparateConnectionHistory(t *testing.T) {
	ctx := t.Context()
	db, err := openHistory(ctx, filepath.Join(t.TempDir(), "flows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	src, dst, proto, sourcePort, destinationPort, id := net.ParseIP("192.0.2.2"), net.ParseIP("198.51.100.2"), uint8(6), uint16(20000), uint16(443), uint32(1)
	entry := conntrack.Con{ID: &id, Timestamp: &conntrack.Timestamp{Start: &now}, Origin: &conntrack.IPTuple{Src: &src, Dst: &dst, Proto: &conntrack.ProtoTuple{Number: &proto, SrcPort: &sourcePort, DstPort: &destinationPort}}}
	first := trafficFlow(entry, now.Add(time.Second))
	applyClassification(&first, dpi.ClassifyResult{AppName: "tls", TLSSNI: "old.routerd.test"})
	if err := syncActive(ctx, db, []logstore.TrafficFlow{first}, []string{first.FlowKey}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	started := now.Add(2 * time.Second)
	entry.Timestamp.Start = &started
	next := trafficFlow(entry, now.Add(3*time.Second))
	applyClassification(&next, dpi.ClassifyResult{AppName: "http", HTTPHost: "new.routerd.test"})
	if err := syncActive(ctx, db, []logstore.TrafficFlow{next}, []string{next.FlowKey}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var count, ended int
	if err := db.QueryRowContext(ctx, "SELECT count(*),count(ts_ended) FROM flows").Scan(&count, &ended); err != nil {
		t.Fatal(err)
	}
	var start int64
	var sni, host string
	if err := db.QueryRowContext(ctx, "SELECT ts_started,tls_sni,http_host FROM flows WHERE flow_key=?", next.FlowKey).Scan(&start, &sni, &host); err != nil {
		t.Fatal(err)
	}
	if count != 2 || ended != 1 || first.FlowKey == next.FlowKey || start != started.UnixNano() || sni != "" || host != "new.routerd.test" {
		t.Fatal("reused connection histories mixed", count, ended, start, sni, host)
	}
	// Existing, untimestamped connections keep one unknown generation instead
	// of being split every time their cumulative counters are sampled.
	entry.Timestamp = nil
	a, b := trafficFlow(entry, now), trafficFlow(entry, now.Add(time.Second))
	if a.FlowKey != b.FlowKey {
		t.Fatal("unknown generation changed with observation time")
	}
}
