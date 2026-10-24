//go:build linux && integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// This smaller counterfactual proves actual missing delivery on the previous
// daemon; it does not stop at the unhealthy-ACK difference in the full fixture.
func TestDaemonRetiredHealthHistoryWithoutReactivation(t *testing.T) {
	if !isolateFixture(t, "TestDaemonRetiredHealthHistoryWithoutReactivation") {
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
			_ = c.Close()
		}
	}()
	root := t.TempDir()
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: "tcp", Port: listener.Addr().(*net.TCPAddr).Port, IntervalMillis: 100, TimeoutMillis: 100}}}}}
	f := startFixtureDaemonAt(t, doc, root)
	ctx := context.Background()
	injector, err := sql.Open("sqlite", filepath.Join(root, "state", "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_retired_delivery BEFORE INSERT ON events WHEN json_extract(CAST(NEW.body AS TEXT),'$.source')='routerd-healthcheck' BEGIN SELECT RAISE(ABORT,'injected stopped relay target failure'); END`); err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Pending []events.Event `json:"pending"`
	}
	expected := map[string]bool{}
	until(t, 5*time.Second, func() bool {
		uri := url.URL{Scheme: "file", Path: filepath.Join(root, "state", "services", "wan", "health-events", "journal.sqlite3"), RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", uri.String())
		if err != nil {
			return false
		}
		defer db.Close()
		var body []byte
		if db.QueryRow(`SELECT body FROM producers WHERE name='health-probe-relay'`).Scan(&body) != nil || json.Unmarshal(body, &pending) != nil {
			return false
		}
		expected = map[string]bool{}
		for _, event := range pending.Pending {
			var saved int
			if injector.QueryRow(`SELECT count(*) FROM events WHERE event_id=?`, event.ID).Scan(&saved) != nil {
				return false
			}
			if saved == 0 {
				expected[event.ID] = true
			}
		}
		return len(expected) > 0 // already-saved/unACKed prefixes cannot satisfy this proof
	})
	var state control.State
	if err = f.control.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	if err = f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "retire-without-reactivation", Config: config.Document{SchemaVersion: config.SchemaVersion}}, nil); err != nil {
		t.Fatal(err)
	}
	noWorker := func() bool {
		var status supervisor.Status
		if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status) != nil {
			return false
		}
		for _, m := range status.Modules {
			if m.Role == "health" {
				return false
			}
		}
		return true
	}
	until(t, 5*time.Second, noWorker)
	if _, err = injector.Exec(`DROP TRIGGER fail_retired_delivery`); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		seen := map[string]int{}
		after := uint64(0)
		for {
			var page control.ObservationPage
			if err = f.control.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
				return false
			}
			for _, record := range page.Records {
				if expected[record.Event.ID] {
					seen[record.Event.ID]++
				}
			}
			if !page.More {
				break
			}
			after = page.NextCursor
		}
		for id := range expected {
			if seen[id] != 1 {
				return false
			}
		}
		return true
	})
	if !noWorker() {
		t.Fatal("queue drainage reactivated the removed producer")
	}
	t.Logf("%d accepted observations delivered once after permanent removal, without producer reactivation", len(expected))
}
