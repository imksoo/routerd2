//go:build linux && integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDerivedHealthCurrentStatusAndDurability(t *testing.T) {
	if !isolateFixture(t, "TestDaemonDerivedHealthCurrentStatusAndDurability") {
		return
	}
	peerNS := fixturePeerNamespace(t, "wan0", "peer0")
	peer, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	link, err := peer.LinkByName("peer0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("192.0.2.1/24")
	if err = peer.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	startPeer := func() {
		t.Helper()
		if err := fixtureInsideNamespace(peerNS, func() error { var err error; listener, err = net.Listen("tcp4", "192.0.2.1:18089"); return err }); err != nil {
			t.Fatal(err)
		}
		current := listener
		go func() {
			for {
				connection, err := current.Accept()
				if err != nil {
					return
				}
				_ = connection.Close()
			}
		}()
	}
	stopPeer := func() {
		t.Helper()
		if listener != nil {
			_ = listener.Close()
			listener = nil
		}
	}
	defer stopPeer()
	derivedSpec := api.DerivedEventSpec{Topic: "routerd.fixture.health", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.phase}", Equals: "Healthy"}}, EmitInitial: true, Hysteresis: "500ms"}
	freshSpec := api.DerivedEventSpec{Topic: "routerd.fixture.fresh", Inputs: derivedSpec.Inputs}
	fresh := &resourceprogram.FactCondition{Any: []resourceprogram.FactCondition{{Fact: "service.wan.active", MaxAge: "1s"}, {Not: &resourceprogram.FactCondition{Fact: "service.wan.active", MaxAge: "1s"}}}}
	rule := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "routerd.fixture.health.asserted", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.twice"}}
	doc := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "port", Interface: &network.Interface{Name: "wan0"}},
		{ID: "address", Address: &resourceprogram.Address{Interface: "wan0", CIDR: "192.0.2.2/24"}},
		{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", SourceAddress: "192.0.2.2", Target: "192.0.2.1", Protocol: "tcp", Port: 18089, IntervalMillis: 100, TimeoutMillis: 100, HealthyThreshold: 1, UnhealthyThreshold: 1}}},
		{ID: "derived", DerivedEvent: &derivedSpec},
		{ID: "fresh", DerivedEvent: &freshSpec, When: fresh},
		{ID: "twice", EventRule: &rule},
	}}
	root := t.TempDir()
	f := startFixtureDaemonAt(t, doc, root)
	ctx := context.Background()
	view := func(id string) derivedcontroller.Observation {
		t.Helper()
		var out derivedcontroller.Observation
		if err := f.control.Do(ctx, "GET", "/v1/derived/"+id, nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	history := func() []events.Record {
		t.Helper()
		var out []events.Record
		after := uint64(0)
		for {
			var page control.ObservationPage
			if err := f.control.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
				t.Fatal(err)
			}
			out = append(out, page.Records...)
			if !page.More {
				return out
			}
			after = page.NextCursor
		}
	}
	count := func(topic string) int {
		t.Helper()
		n := 0
		for _, record := range history() {
			if record.Event.Topic == topic {
				n++
			}
		}
		return n
	}
	instance := func() runtimeplan.InstanceStatus {
		t.Helper()
		var out runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, m := range status.Modules {
			if m.Name == name {
				return m
			}
		}
		t.Fatal("missing worker", name)
		return supervisor.ModuleStatus{}
	}
	restart := func(name string) {
		t.Helper()
		old := module(name)
		if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 20*time.Second, func() bool {
			var status supervisor.Status
			if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status) != nil || !status.Ready {
				return false
			}
			for _, m := range status.Modules {
				if m.Name == name {
					return m.Ready && m.PID != old.PID && m.Generation > old.Generation
				}
			}
			return false
		})
	}
	until(t, 15*time.Second, func() bool {
		v := view("derived")
		return v.State.Asserted != nil && !*v.State.Asserted && count("routerd.fixture.health.retracted") == 1
	})
	startPeer()
	until(t, 15*time.Second, func() bool {
		v := view("derived")
		return v.State.Asserted != nil && *v.State.Asserted && count("routerd.fixture.health.asserted") == 1
	})
	restart("logging")
	stopPeer()
	until(t, 15*time.Second, func() bool {
		v := view("derived")
		return v.State.Asserted != nil && !*v.State.Asserted && count("routerd.fixture.health.retracted") == 2
	})
	startPeer()
	until(t, 15*time.Second, func() bool {
		return count("routerd.fixture.health.asserted") == 2 && count("routerd.fixture.twice") == 1
	})
	// Freeze accepted health observations without changing supervisor readiness.
	victim := module("service.wan")
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	healthStopped := true
	defer func() {
		if healthStopped {
			_ = syscall.Kill(victim.PID, syscall.SIGCONT)
		}
	}()
	var frozen health.Snapshot
	if err := json.Unmarshal(instance().Snapshot, &frozen); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool { v := view("fresh"); return v.State.Asserted == nil && v.State.Phase == "Inactive" })
	until(t, 5*time.Second, func() bool {
		before := instance()
		v := view("derived")
		after := instance()
		if before.Sequence != after.Sequence {
			return false
		}
		if err := json.Unmarshal(after.Snapshot, &frozen); err != nil {
			t.Fatal(err)
		}
		if v.ObservedNanos != 0 && v.ObservedNanos != frozen.ObservedNanos {
			t.Fatal("polling manufactured a new observation time", v.ObservedNanos, frozen.ObservedNanos)
		}
		return true
	})
	if err := syscall.Kill(victim.PID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	healthStopped = false
	until(t, 10*time.Second, func() bool { v := view("fresh"); return v.State.Asserted != nil && *v.State.Asserted })
	stale := healthPBRUpdate(instance())
	restart("service.wan")
	until(t, 15*time.Second, func() bool { return instance().Generation > stale.Generation && view("derived").State.Asserted != nil })
	if err = f.control.Do(ctx, "POST", "/v1/services/state", stale, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("old health notification was accepted", err)
	}
	restart("control")
	until(t, 15*time.Second, func() bool { v := view("derived"); return v.State.Asserted != nil && *v.State.Asserted })
	if count("routerd.fixture.health.asserted") != 2 || count("routerd.fixture.twice") != 1 {
		t.Fatal("worker restart replayed an old assertion")
	}
	// A durable state write fails before any new retraction is published.
	injector, err := sql.Open("sqlite", filepath.Join(root, "state", "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`PRAGMA busy_timeout=1000; CREATE TRIGGER fail_live_derived BEFORE UPDATE ON producers WHEN NEW.name LIKE 'derived/%' BEGIN SELECT RAISE(ABORT,'injected derived state failure'); END`); err != nil {
		t.Fatal(err)
	}
	stopPeer()
	until(t, 10*time.Second, func() bool {
		request, _ := http.NewRequestWithContext(ctx, "GET", "http://routerd/v1/health", nil)
		response, err := f.control.HTTP.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var h control.Health
		return response.StatusCode == 503 && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&h) == nil && h.Derived.LastError != ""
	})
	if err = f.control.Do(ctx, "GET", "/v1/derived/derived", nil, nil); !localapi.IsHTTPStatus(err, 503) {
		t.Fatal("failed evaluator exposed a current assertion", err)
	}
	if count("routerd.fixture.health.retracted") != 2 {
		t.Fatal("failed state save leaked a retraction")
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_live_derived`); err != nil {
		t.Fatal(err)
	}
	until(t, 15*time.Second, func() bool {
		var h control.Health
		if f.control.Do(ctx, "GET", "/v1/health", nil, &h) != nil {
			return false
		}
		return h.Healthy && count("routerd.fixture.health.retracted") == 3
	})
	t.Log("real TCP probes -> finite typed current status -> durable DerivedEvent -> EventRule; producer/control/logging restart, old notification, max_age and SQL failure recovery passed")
	t.Log("ROUTERD_DERIVED_DAEMON_TEST_PASS")
}
