// SPDX-License-Identifier: BSD-3-Clause
package trafficflow

import (
	"fmt"
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
