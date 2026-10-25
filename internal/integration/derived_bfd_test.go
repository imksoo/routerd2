//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestDaemonBFDDerivedCurrentStatus(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBFDDerivedCurrentStatus") {
		return
	}
	peer, _, _ := newDaemonBFDPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failures := make(chan error, 4)
	go peer.run(ctx, failures)
	spec := api.DerivedEventSpec{Topic: "routerd.fixture.bfd", EmitInitial: true, Hysteresis: "650ms", Inputs: []api.ReadyWhenSpec{{Field: "${BFD/service/watch.status.phase}", Equals: "Up"}, {Field: "${BFD/service/watch.status.peers}", NotEmpty: true}, {Field: "${BFD/service/watch.status.peerStates}", NotEmpty: true}, {Field: "${BFD/service/watch.status.observedAt}", NotEmpty: true}}}
	rule := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "routerd.fixture.bfd.asserted", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.bfd.twice"}}
	doc := config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, Services: []serviceconfig.Service{{Name: "watch", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2", MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}}}})
	doc.Resources = append(doc.Resources, resourceprogram.Resource{ID: "bfd-derived", DerivedEvent: &spec}, resourceprogram.Resource{ID: "bfd-rule", EventRule: &rule})
	f := startFixtureDaemon(t, doc)
	view := func() derivedcontroller.Observation {
		t.Helper()
		var out derivedcontroller.Observation
		if err := f.control.Do(ctx, "GET", "/v1/derived/bfd-derived", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	count := func(topic string) int {
		t.Helper()
		after := uint64(0)
		n := 0
		for {
			var page control.ObservationPage
			if err := f.control.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Records {
				if r.Event.Topic == topic {
					n++
				}
			}
			if !page.More {
				return n
			}
			after = page.NextCursor
		}
	}
	status := func() (runtimeplan.InstanceStatus, bfd.Snapshot) {
		t.Helper()
		var out runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.watch", nil, &out); err != nil {
			t.Fatal(err)
		}
		var snapshot bfd.Snapshot
		if err := json.Unmarshal(out.Snapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		return out, snapshot
	}
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var out supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &out); err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Modules {
			if m.Name == name {
				return m
			}
		}
		t.Fatal("module missing", name)
		return supervisor.ModuleStatus{}
	}
	restart := func(name string) {
		t.Helper()
		old := module(name)
		if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 20*time.Second, func() bool {
			var current supervisor.Status
			if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &current) != nil || !current.Ready {
				return false
			}
			for _, m := range current.Modules {
				if m.Name == name && m.Ready && m.PID != old.PID && m.Generation > old.Generation {
					return true
				}
			}
			return false
		})
	}
	assert := func(up bool) {
		t.Helper()
		until(t, 12*time.Second, func() bool {
			select {
			case err := <-failures:
				t.Fatal("independent UDP peer", err)
			default:
			}
			v := view()
			return v.State.Asserted != nil && *v.State.Asserted == up
		})
	}
	assert(false)
	peer.enabled.Store(true)
	assert(true)
	until(t, 10*time.Second, func() bool { return count(spec.Topic+".asserted") == 1 })
	peer.enabled.Store(false)
	assert(false)
	peer.enabled.Store(true)
	assert(true)
	until(t, 10*time.Second, func() bool { return count(spec.Topic+".asserted") == 2 && count("routerd.fixture.bfd.twice") == 1 })
	before, raw := status()
	// A real producer must carry timestamp continuity across more than one
	// periodic status interval, without cancelling notEmpty's longer hold.
	if raw.TimestampChanges == 0 {
		t.Fatal("real BFD producer omitted timestamp continuity")
	}
	until(t, 5*time.Second, func() bool {
		_, next := status()
		return next.TimestampChanges > raw.TimestampChanges && next.Peers[0].State == bfd.StateUp
	})
	oldWorker := module("service.watch")
	if raw.Peers[0].State != bfd.StateUp || peer.received.Load() == 0 {
		t.Fatal("no actual Up exchange")
	}
	if err := syscall.Kill(oldWorker.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	stopped := true
	defer func() {
		if stopped {
			_ = syscall.Kill(oldWorker.PID, syscall.SIGCONT)
		}
	}()
	until(t, 5*time.Second, func() bool { return view().State.Asserted == nil })
	frozen, sample := status()
	ready := module("service.watch")
	if sample.Peers[0].State != bfd.StateUp || ready.PID != oldWorker.PID || !ready.Ready {
		t.Fatal("worker exit or Down message masked independent expiry", ready, sample)
	}
	var manager supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &manager); err != nil {
		t.Fatal(err)
	}
	replay := serviceconfig.Update{SupervisorInstance: manager.InstanceID, Name: "service.watch", Role: "bfd", ConfigDigest: frozen.ConfigDigest, Generation: frozen.Generation, ProducerEpoch: frozen.ProducerEpoch, Sequence: frozen.Sequence, Snapshot: frozen.Snapshot}
	if err := f.control.Do(ctx, "POST", "/v1/services/state", replay, nil); err != nil {
		t.Fatal("identical accepted retry failed", err)
	}
	if v := view(); v.State.Asserted != nil {
		t.Fatal("replay renewed expired Up", v)
	}
	if err := syscall.Kill(oldWorker.PID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	stopped = false
	assert(true)
	restart("service.watch")
	assert(true)
	if err := f.control.Do(ctx, "POST", "/v1/services/state", replay, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("old BFD worker notification accepted", err)
	}
	current, _ := status()
	if current.Generation <= before.Generation || current.ProducerEpoch == before.ProducerEpoch {
		t.Fatal("worker identity did not rotate", current)
	}
	// Stored asserted history cannot supply current Up after control recovery.
	peer.enabled.Store(false)
	victim := module("service.watch")
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(victim.PID, syscall.SIGCONT)
	restart("control")
	until(t, 5*time.Second, func() bool { v := view(); return v.State.Asserted == nil || !*v.State.Asserted })
	for untilAt := time.Now().Add(300 * time.Millisecond); time.Now().Before(untilAt); {
		if v := view(); v.State.Asserted != nil && *v.State.Asserted {
			t.Fatal("durable old Up became current after control restart", v)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(victim.PID, syscall.SIGCONT)
	peer.enabled.Store(true)
	assert(true)
	t.Log("independent real UDP BFD -> four typed fields -> durable DerivedEvent/EventRule; expiry, duplicate replay, worker/control restart and stale notification fencing passed")
	t.Log("ROUTERD_BFD_DERIVED_DAEMON_TEST_PASS")
}
