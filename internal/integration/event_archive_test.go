//go:build linux && integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
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
	t.Logf("actual CLI exported and independently matched %d complete records through cursor %d, verified offline and preserved source producer authority", exported.Records, exported.Header.TipCursor)
}
