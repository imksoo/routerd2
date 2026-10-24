//go:build linux && integration

package integration

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestDaemonEventArchiveAndCapacity(t *testing.T) {
	if !isolateFixture(t, "TestDaemonEventArchiveAndCapacity") {
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	root := t.TempDir()
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: "tcp", Port: listener.Addr().(*net.TCPAddr).Port, IntervalMillis: 100, TimeoutMillis: 100}}}}}
	source.Resources = append(source.Resources,
		resourceprogram.Resource{ID: "archive-derived", DerivedEvent: &api.DerivedEventSpec{Topic: "routerd.fixture.archive-state", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.phase}", Equals: "Healthy"}}, EmitInitial: true}},
		resourceprogram.Resource{ID: "archive-probes", EventRule: &api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "routerd.healthcheck.wan.passed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.archive-probes"}}},
	)
	f := startFixtureDaemonAt(t, source, root)
	ctx := context.Background()
	var storage control.EventStoreHealth
	if err = f.control.Do(ctx, "GET", "/v1/event-store", nil, &storage); err != nil || storage.Capacity == nil {
		t.Fatal("real event-store inspection unavailable", err)
	}
	until(t, 10*time.Second, func() bool {
		if f.control.Do(ctx, "GET", "/v1/event-store", nil, &storage) != nil {
			return false
		}
		return storage.Capacity != nil && storage.Capacity.Events.Rows >= 3
	})
	var before runtimeplan.InstanceStatus
	if err = f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &before); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd")
	path := filepath.Join(root, "history.jsonl")
	output, err := exec.CommandContext(ctx, binary, "events", "export", "--runtime-dir", filepath.Join(root, "run"), "--output", path).CombinedOutput()
	var exported events.ArchiveSummary
	if err != nil || json.Unmarshal(output, &exported) != nil || exported.Records < 3 {
		t.Fatal("actual command export failed", err, string(output))
	}
	output, err = exec.CommandContext(ctx, binary, "events", "verify", path).CombinedOutput()
	var verified events.ArchiveSummary
	if err != nil || json.Unmarshal(output, &verified) != nil || verified != exported {
		t.Fatal("actual offline verify failed", err, string(output))
	}
	actual := map[uint64]events.Event{}
	after := uint64(0)
	for after < exported.Header.TipCursor {
		var page control.ObservationPage
		if err = f.control.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Records) == 0 {
			t.Fatal("export prefix disappeared")
		}
		for _, record := range page.Records {
			if record.Cursor <= exported.Header.TipCursor {
				actual[record.Cursor] = record.Event
			}
		}
		after = page.NextCursor
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	count := int64(0)
	for scanner.Scan() {
		var line struct {
			Kind   string       `json:"kind"`
			Cursor uint64       `json:"cursor"`
			Event  events.Event `json:"event"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Kind == "event" {
			if !reflect.DeepEqual(line.Event, actual[line.Cursor]) {
				t.Fatal("archive content differs from authoritative source", line.Cursor)
			}
			count++
		}
	}
	if scanner.Err() != nil || count != exported.Records {
		t.Fatal("archive record framing mismatch", count, scanner.Err())
	}
	if info, err := file.Stat(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("archive is not private", info, err)
	}
	if _, err = exec.CommandContext(ctx, binary, "events", "export", "--runtime-dir", filepath.Join(root, "run"), "--output", path).CombinedOutput(); err == nil {
		t.Fatal("actual command overwrote an existing archive")
	}
	until(t, 10*time.Second, func() bool {
		var current runtimeplan.InstanceStatus
		return f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &current) == nil && current.Generation == before.Generation && current.ProducerEpoch == before.ProducerEpoch && current.Sequence > before.Sequence
	})
	if os.Getenv("ROUTERD2_EXPECT_CAPACITY_WARNING") == "1" {
		until(t, 10*time.Second, func() bool {
			var h control.Health
			return f.control.Do(ctx, "GET", "/v1/health", nil, &h) == nil && h.Healthy && h.EventJournal.Storage.Capacity != nil && h.EventJournal.Storage.Capacity.Pressure == "warning" && slices.Contains(h.EventJournal.Storage.Capacity.Reasons, "filesystem_space")
		})
		t.Log("actual low-space warning visible while real Health producer and control remain healthy")
	}
	verifyDaemonCompletedCompaction(t, f, root)
	t.Logf("actual CLI exported and independently matched %d complete records through cursor %d, verified offline and preserved source producer authority", exported.Records, exported.Header.TipCursor)
}

// This uses actual consumer and producer outputs, not injected completed rows.
func verifyDaemonCompletedCompaction(t *testing.T, f daemonFixture, root string) {
	t.Helper()
	ctx := context.Background()
	var storage control.EventStoreHealth
	until(t, 12*time.Second, func() bool {
		return f.control.Do(ctx, "GET", "/v1/event-store", nil, &storage) == nil && storage.Capacity != nil && storage.Capacity.Tombstones.Rows >= 2
	})
	db, err := sql.Open("sqlite", filepath.Join(root, "state", "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA busy_timeout=1000`); err != nil {
		t.Fatal(err)
	}
	type witness struct {
		kind      int
		name, key string
		position  uint64
		digest    []byte
	}
	var samples []witness
	for _, kind := range []int{0, 1} {
		w := witness{kind: kind}
		if err = db.QueryRow(`SELECT name,key,position,digest FROM completed_jobs WHERE kind=? ORDER BY name,key LIMIT 1`, kind).Scan(&w.name, &w.key, &w.position, &w.digest); err != nil || len(w.digest) != 32 || w.position == 0 {
			t.Fatal("actual output compaction missing", kind, err)
		}
		samples = append(samples, w)
	}
	var overlap int
	if err = db.QueryRow(`SELECT count(*) FROM completed_jobs d WHERE (d.kind=0 AND EXISTS(SELECT 1 FROM outbox o WHERE o.consumer=d.name AND o.key=d.key)) OR (d.kind=1 AND EXISTS(SELECT 1 FROM producer_outbox p WHERE p.producer=d.name AND p.key=d.key))`).Scan(&overlap); err != nil || overlap != 0 {
		t.Fatal("completed payload was not reclaimed", overlap, err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_compaction BEFORE INSERT ON completed_jobs BEGIN SELECT RAISE(ABORT,'injected completed work failure'); END`); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		return f.control.Do(ctx, "GET", "/v1/event-store", nil, &storage) == nil && storage.CompactionError != "" && storage.LastError != ""
	})
	var h control.Health
	if err = f.control.Do(ctx, "GET", "/v1/health", nil, &h); !localapi.IsHTTPStatus(err, 503) {
		t.Fatal("actual maintenance error was healthy", err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_compaction`); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		return f.control.Do(ctx, "GET", "/v1/event-store", nil, &storage) == nil && storage.CompactionError == "" && f.control.Do(ctx, "GET", "/v1/health", nil, &h) == nil && h.Healthy
	})
	var before supervisor.Status
	if err = f.supervisor.Do(ctx, "GET", "/v1/status", nil, &before); err != nil {
		t.Fatal(err)
	}
	var old supervisor.ModuleStatus
	for _, m := range before.Modules {
		if m.Name == "control" {
			old = m
		}
	}
	if old.PID == 0 {
		t.Fatal("actual control worker missing")
	}
	if err = syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 20*time.Second, func() bool {
		var current supervisor.Status
		if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &current) != nil || !current.Ready {
			return false
		}
		for _, m := range current.Modules {
			if m.Name == "control" && m.Ready && m.PID != old.PID && m.Generation > old.Generation {
				return f.control.Do(ctx, "GET", "/v1/health", nil, &h) == nil && h.Healthy
			}
		}
		return false
	})
	for _, w := range samples {
		var position uint64
		var digest []byte
		if err = db.QueryRow(`SELECT position,digest FROM completed_jobs WHERE kind=? AND name=? AND key=?`, w.kind, w.name, w.key).Scan(&position, &digest); err != nil || position != w.position || !reflect.DeepEqual(digest, w.digest) {
			t.Fatal("SIGKILL changed completed identity", err)
		}
	}
	t.Log("actual EventRule and Derived outputs compacted; injected write failure stayed unhealthy across inspection; recovery and control SIGKILL preserved original dedup identity")
}
