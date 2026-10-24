//go:build linux && integration

package integration

import (
	"context"
	"fmt"
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
	f := startFixtureDaemon(t, doc)
	read := func() control.Health {
		t.Helper()
		var h control.Health
		if err := f.control.Do(ctx, "GET", "/v1/health", nil, &h); err != nil {
			t.Fatal(err)
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
		h := read()
		request := control.CommitRequest{ExpectedRevision: h.Revision, RequestID: id, Config: d}
		if confirmed {
			request.ConfirmTimeoutSeconds = 5
		}
		if err := f.control.Do(ctx, "POST", "/v1/config/commit", request, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitConsumed := func(name string) {
		t.Helper()
		until(t, 10*time.Second, func() bool {
			h := read()
			for _, r := range h.EventRules.Rules {
				if r.ID == name && r.Cursor > 0 {
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
	timer := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "debounce", Topic: "configuration.committed", Duration: "3s"}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.timer"}}
	withTimer := config.Document{SchemaVersion: config.SchemaVersion, Resources: append(append([]resourceprogram.Resource(nil), doc.Resources...), resourceprogram.Resource{ID: "timer", EventRule: &timer})}
	commit("two", withTimer, false)
	waitConsumed("timer")
	until(t, 10*time.Second, func() bool { return len(outputs("routerd.fixture.count")) == 1 })
	restart("control")
	until(t, 10*time.Second, func() bool { return len(outputs("routerd.fixture.timer")) == 1 })
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
	time.Sleep(3300 * time.Millisecond)
	if len(outputs("routerd.fixture.timer")) != 1 {
		t.Fatal("removed timer published")
	}
	commit("confirmed-trial", withTimer, true)
	waitConsumed("timer")
	restart("control")
	until(t, 10*time.Second, func() bool {
		var state control.State
		if f.control.Do(ctx, "GET", "/v1/config", nil, &state) != nil {
			return false
		}
		return state.Confirmation == nil && len(state.Config.Resources) == 1
	})
	time.Sleep(3300 * time.Millisecond)
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
	t.Log("ROUTERD_EVENTRULE_DAEMON_TEST_PASS")
}
