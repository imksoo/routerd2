//go:build linux && integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestDaemonEventRuleDurabilityAndLifecycle(t *testing.T) {
	if !isolateFixture(t, "TestDaemonEventRuleDurabilityAndLifecycle") {
		return
	}
	ctx := context.Background()
	count := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "configuration.committed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.count", Attributes: map[string]string{"count": "${count}"}}}
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "count", EventRule: &count}}}
	stateRoot := t.TempDir()
	f := startFixtureDaemonAt(t, doc, stateRoot)
	read := func() control.Health {
		t.Helper()
		var h control.Health
		request, err := http.NewRequestWithContext(ctx, "GET", "http://routerd/v1/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := f.control.HTTP.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 503 {
			t.Fatal("unexpected health status", response.StatusCode)
		}
		if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&h); err != nil {
			t.Fatal(err)
		}
		if !h.Healthy {
			t.Logf("health during convergence: services=%+v event_rules=%s journal=%s", h.Services, h.EventRules.LastError, h.EventJournal.LastError)
		}
		return h
	}
	history := func() []events.Record {
		t.Helper()
		var records []events.Record
		after := uint64(0)
		for {
			var page control.ObservationPage
			if err := f.control.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
				t.Fatal(err)
			}
			records = append(records, page.Records...)
			if !page.More {
				return records
			}
			after = page.NextCursor
		}
	}
	outputs := func(topic string) []events.Record {
		t.Helper()
		var out []events.Record
		for _, r := range history() {
			if r.Event.Topic == topic {
				out = append(out, r)
			}
		}
		return out
	}
	commit := func(id string, d config.Document, confirmed bool) {
		t.Helper()
		var h control.Health
		until(t, 10*time.Second, func() bool { h = read(); return h.Healthy })
		request := control.CommitRequest{ExpectedRevision: h.Revision, RequestID: id, Config: d}
		if confirmed {
			request.ConfirmTimeoutSeconds = 30
		}
		if err := f.control.Do(ctx, "POST", "/v1/config/commit", request, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitConsumed := func(name string) {
		t.Helper()
		until(t, 10*time.Second, func() bool {
			h := read()
			cursor := uint64(0)
			for _, record := range history() {
				if record.Event.Topic == "configuration.committed" && record.Event.Attributes["revision"] == fmt.Sprint(h.Revision) {
					cursor = record.Cursor
				}
			}
			for _, r := range h.EventRules.Rules {
				if r.ID == name && cursor > 0 && r.Cursor >= cursor {
					return true
				}
			}
			return false
		})
	}
	restart := func(name string) {
		t.Helper()
		var before supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &before); err != nil {
			t.Fatal(err)
		}
		var victim supervisor.ModuleStatus
		for _, m := range before.Modules {
			if m.Name == name {
				victim = m
			}
		}
		if victim.PID == 0 {
			t.Fatal("missing worker", name)
		}
		if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 15*time.Second, func() bool {
			var s supervisor.Status
			if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &s) != nil || !s.Ready {
				return false
			}
			for _, m := range s.Modules {
				if m.Name == name {
					return m.Ready && m.PID != victim.PID && m.Generation > victim.Generation
				}
			}
			return false
		})
	}
	commit("one", doc, false)
	waitConsumed("count")
	if len(outputs("routerd.fixture.count")) != 0 {
		t.Fatal("count emitted after one input")
	}
	restart("logging")
	restart("control")
	timer := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "debounce", Topic: "configuration.committed", Duration: "8s"}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.timer"}}
	withTimer := config.Document{SchemaVersion: config.SchemaVersion, Resources: append(append([]resourceprogram.Resource(nil), doc.Resources...), resourceprogram.Resource{ID: "timer", EventRule: &timer})}
	commit("two", withTimer, false)
	waitConsumed("timer")
	until(t, 10*time.Second, func() bool { return len(outputs("routerd.fixture.count")) == 1 })
	if len(outputs("routerd.fixture.timer")) != 0 {
		t.Fatal("timer fired before the intended control interruption")
	}
	restart("control")
	until(t, 15*time.Second, func() bool { return len(outputs("routerd.fixture.timer")) == 1 })
	for _, r := range outputs("routerd.fixture.count") {
		if r.Event.Detail == nil || r.Event.Detail.Resource == nil || r.Event.Detail.Resource.Kind != "EventRule" || r.Event.Attributes["count"] != "2" {
			t.Fatal("typed output lost", r)
		}
	}
	commit("arm-then-remove", withTimer, false)
	until(t, 10*time.Second, func() bool {
		h := read()
		for _, r := range h.EventRules.Rules {
			if r.ID == "timer" {
				for _, record := range history() {
					if record.Event.Topic == "configuration.committed" && record.Event.Attributes["revision"] == fmt.Sprint(h.Revision) {
						return r.Cursor >= record.Cursor
					}
				}
			}
		}
		return false
	})
	commit("remove", doc, false)
	time.Sleep(8300 * time.Millisecond)
	if len(outputs("routerd.fixture.timer")) != 1 {
		t.Fatal("removed timer published")
	}
	commit("confirmed-trial", withTimer, true)
	waitConsumed("timer")
	if len(outputs("routerd.fixture.timer")) != 1 {
		t.Fatal("trial timer was not pending before interruption")
	}
	restart("control")
	until(t, 10*time.Second, func() bool {
		var state control.State
		if f.control.Do(ctx, "GET", "/v1/config", nil, &state) != nil {
			return false
		}
		return state.Confirmation == nil && len(state.Config.Resources) == 1
	})
	if len(outputs("configuration.rolled_back_restart")) != 1 {
		t.Fatal("trial did not take the control-restart rollback path")
	}
	time.Sleep(8300 * time.Millisecond)
	if len(outputs("routerd.fixture.timer")) != 1 {
		t.Fatal("confirmed rollback revived trial timer")
	}
	before := history()
	restart("control")
	after := history()
	if len(before) != len(after) {
		t.Fatal("restart duplicated historical output", len(before), len(after))
	}
	for i := range before {
		if before[i].Event.ID != after[i].Event.ID || before[i].Cursor != after[i].Cursor {
			t.Fatal("history identity changed")
		}
	}
	injector, err := sql.Open("sqlite", filepath.Join(stateRoot, "state", "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`PRAGMA busy_timeout=1000; CREATE TRIGGER fail_live_registry BEFORE UPDATE ON producers WHEN NEW.name='control-eventrule-lifecycles' BEGIN SELECT RAISE(ABORT,'injected live registry failure'); END`); err != nil {
		t.Fatal(err)
	}
	revision := read().Revision
	var ack control.State
	if err = f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: revision, RequestID: "committed-with-controller-backpressure", Config: doc}, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Revision != revision+1 || ack.Healthy || ack.EventRuleError == "" {
		t.Fatal("live commit hid post-save controller failure", ack)
	}
	if h := read(); h.Healthy || h.EventRules.LastError == "" {
		t.Fatal("live health hid controller failure", h)
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_live_registry`); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool { return read().Healthy })
	waitConsumed("count")
	until(t, 10*time.Second, func() bool { return len(outputs("routerd.fixture.count")) == 3 })
	t.Log("actual SQLite lifecycle write failure preserved the committed revision, returned unhealthy ACK/health and recovered without losing or duplicating inputs")
	t.Log("ROUTERD_EVENTRULE_DAEMON_TEST_PASS")
}
