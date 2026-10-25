//go:build linux && integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestDaemonHealthContinuesWithFullHistory(t *testing.T) {
	if !isolateFixture(t, "TestDaemonHealthContinuesWithFullHistory") {
		return
	}
	ctx := context.Background()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	serve := func(listener net.Listener) {
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				_ = connection.Close()
			}
		}()
	}
	serve(listener)
	defer func() { _ = listener.Close() }()
	root := t.TempDir()
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "lo", Target: "127.0.0.1", Protocol: "tcp", Port: port, IntervalMillis: 100, TimeoutMillis: 100, HealthyThreshold: 1, UnhealthyThreshold: 1}}}}}
	f := startFixtureDaemonAt(t, doc, root)
	sample := func() (health.Snapshot, uint64) {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &instance); err != nil {
			t.Fatal(err)
		}
		var observation health.Snapshot
		if len(instance.Snapshot) > 0 {
			if err := json.Unmarshal(instance.Snapshot, &observation); err != nil {
				t.Fatal(err)
			}
		}
		return observation, instance.Sequence
	}
	until(t, 5*time.Second, func() bool { s, _ := sample(); return s.Active })
	injector, err := sql.Open("sqlite", filepath.Join(root, "state", "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`PRAGMA busy_timeout=2000; CREATE TRIGGER stop_health_history BEFORE INSERT ON events WHEN json_extract(CAST(NEW.body AS TEXT),'$.source')='routerd-healthcheck' BEGIN SELECT RAISE(ABORT,'history destination unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	until(t, 15*time.Second, func() bool { s, _ := sample(); return s.HistoryDropped > 0 && s.HistoryError != "" })
	before, sequence := sample()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		s, next := sample()
		return !s.Active && next > sequence && s.HistoryDropped > before.HistoryDropped
	})
	listener, err = net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	serve(listener)
	until(t, 5*time.Second, func() bool { s, _ := sample(); return s.Active && s.HistoryError != "" })
	if _, err = injector.Exec(`DROP TRIGGER stop_health_history`); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool { s, _ := sample(); return s.Active && s.HistoryError == "" })
	var status supervisor.Status
	if err = f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	for _, module := range status.Modules {
		if module.Name == "service.wan" {
			if !module.Ready || module.Restarts != 0 {
				t.Fatal("history capacity caused a Health worker restart", module)
			}
			t.Log("real TCP failure and recovery continued with a full history queue; loss remained visible and the same worker resumed history delivery")
			return
		}
	}
	t.Fatal("Health worker missing after history recovery")
}
