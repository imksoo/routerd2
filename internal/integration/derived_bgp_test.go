//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type enabledBGPListener struct {
	net.Listener
	enabled *atomic.Bool
}

func (l enabledBGPListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.enabled.Load() {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func TestDaemonBGPDerivedCurrentStatus(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBGPDerivedCurrentStatus") {
		return
	}
	_, listener, index := newDaemonBFDPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enabled atomic.Bool
	var peerMu sync.Mutex
	var current *fixtureBGPPeer
	failures := make(chan error, 4)
	go func() {
		for {
			peer, err := acceptFixturePeer(enabledBGPListener{listener, &enabled})
			if err != nil {
				if ctx.Err() == nil && enabled.Load() {
					select {
					case failures <- err:
					default:
					}
				}
				return
			}
			peerMu.Lock()
			current = peer
			peerMu.Unlock()
			go func() {
				ticker := time.NewTicker(200 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if peer.send(4, nil) != nil {
							return
						}
					}
				}
			}()
			go func() {
				for {
					if _, _, err := readFixtureMessage(peer.conn); err != nil {
						return
					}
				}
			}()
		}
	}()
	defer func() {
		peerMu.Lock()
		defer peerMu.Unlock()
		if current != nil {
			_ = current.conn.Close()
		}
	}()
	spec := api.DerivedEventSpec{Topic: "routerd.fixture.bgp", EmitInitial: true, Hysteresis: "150ms", Inputs: []api.ReadyWhenSpec{{Field: "${BGPRouter/service/router.status.phase}", Equals: "Established"}, {Field: "${BGPRouter/service/router.status.establishedPeers}", Equals: "1"}, {Field: "${BGPRouter/service/router.status.backend}", Equals: "gobgp"}, {Field: "${BGPRouter/service/router.status.observedAt}", NotEmpty: true}}}
	rule := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: spec.Topic + ".asserted", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.bgp.twice"}}
	doc := config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, Services: []serviceconfig.Service{{Name: "router", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 64513, TimersProfile: "fast", ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24"}, NextHopRewrite: "unchanged"}}}}, PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", BindInterface: "wan0", RemotePort: listener.Addr().(*net.TCPAddr).Port}}, FIBInterface: "wan0"}}}})
	doc.Resources = append(doc.Resources, resourceprogram.Resource{ID: "bgp-derived", DerivedEvent: &spec}, resourceprogram.Resource{ID: "bgp-rule", EventRule: &rule})
	f := startFixtureDaemon(t, doc)
	view := func() derivedcontroller.Observation {
		t.Helper()
		var out derivedcontroller.Observation
		if err := f.control.Do(ctx, "GET", "/v1/derived/bgp-derived", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	assert := func(value bool) {
		t.Helper()
		until(t, 12*time.Second, func() bool {
			select {
			case err := <-failures:
				t.Fatal("independent BGP peer", err)
			default:
			}
			v := view()
			return v.State.Asserted != nil && *v.State.Asserted == value
		})
	}
	status := func() (runtimeplan.InstanceStatus, bgpconfig.Snapshot) {
		t.Helper()
		var out runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.router", nil, &out); err != nil {
			t.Fatal(err)
		}
		var raw bgpconfig.Snapshot
		if err := json.Unmarshal(out.Snapshot, &raw); err != nil {
			t.Fatal(err)
		}
		return out, raw
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
		t.Fatal("missing module", name)
		return supervisor.ModuleStatus{}
	}
	restart := func(name string) {
		t.Helper()
		old := module(name)
		if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 20*time.Second, func() bool {
			var out supervisor.Status
			if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &out) != nil || !out.Ready {
				return false
			}
			for _, m := range out.Modules {
				if m.Name == name {
					return m.Ready && m.PID != old.PID && m.Generation > old.Generation
				}
			}
			return false
		})
	}
	count := func(topic string) int {
		t.Helper()
		after, n := uint64(0), 0
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
	assert(false)
	enabled.Store(true)
	assert(true)
	until(t, 5*time.Second, func() bool { return count(spec.Topic+".asserted") == 1 })
	send := func(hop string) {
		t.Helper()
		peerMu.Lock()
		defer peerMu.Unlock()
		if current == nil {
			t.Fatal("no independent peer")
		}
		if err := current.send(2, fixtureUpdate("203.0.113.0/24", hop, false)); err != nil {
			t.Fatal(err)
		}
	}
	send("198.51.100.254")
	assert(false)
	_, degraded := status()
	if len(degraded.Peers) != 1 || degraded.Peers[0].State != "SESSION_STATE_ESTABLISHED" || fixtureRoute(t, "203.0.113.0/24", "", 0) {
		t.Fatal("session loss masked unresolved-FIB degradation", degraded)
	}
	send("192.0.2.2")
	assert(true)
	until(t, 8*time.Second, func() bool {
		return fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", index) && count(spec.Topic+".asserted") == 2 && count("routerd.fixture.bgp.twice") == 1
	})
	before, _ := status()
	victim := module("service.router")
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(victim.PID, syscall.SIGCONT)
	until(t, 8*time.Second, func() bool { return view().State.Asserted == nil && !fixtureRoute(t, "203.0.113.0/24", "", 0) })
	frozen, raw := status()
	if !raw.Active || raw.Peers[0].State != "SESSION_STATE_ESTABLISHED" || module("service.router").PID != victim.PID {
		t.Fatal("worker exit masked independent clock expiry")
	}
	var manager supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &manager); err != nil {
		t.Fatal(err)
	}
	replay := serviceconfig.Update{SupervisorInstance: manager.InstanceID, Name: "service.router", Role: "bgp", ConfigDigest: frozen.ConfigDigest, Generation: frozen.Generation, ProducerEpoch: frozen.ProducerEpoch, Sequence: frozen.Sequence, Snapshot: frozen.Snapshot}
	if err := f.control.Do(ctx, "POST", "/v1/services/state", replay, nil); err != nil {
		t.Fatal("identical retry rejected", err)
	}
	if v := view(); v.State.Asserted != nil {
		t.Fatal("retry renewed expired Established", v)
	}
	_ = syscall.Kill(victim.PID, syscall.SIGCONT)
	assert(true)
	restart("service.router")
	assert(true)
	if err := f.control.Do(ctx, "POST", "/v1/services/state", replay, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("old worker notification accepted", err)
	}
	next, _ := status()
	if next.Generation <= before.Generation || next.ProducerEpoch == before.ProducerEpoch {
		t.Fatal("BGP source identity did not rotate")
	}
	enabled.Store(false)
	peerMu.Lock()
	if current != nil {
		_ = current.conn.Close()
	}
	peerMu.Unlock()
	paused := module("service.router")
	if err := syscall.Kill(paused.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(paused.PID, syscall.SIGCONT)
	restart("control")
	if v := view(); v.State.Asserted != nil && *v.State.Asserted {
		t.Fatal("durable old Established became current", v)
	}
	_ = syscall.Kill(paused.PID, syscall.SIGCONT)
	enabled.Store(true)
	assert(true)
	t.Log("real TCP BGP -> four current fields -> DerivedEvent/EventRule; unresolved FIB, original boot expiry, replay, worker/control restart and generation fence passed")
	t.Log("ROUTERD_BGP_DERIVED_DAEMON_TEST_PASS")
}
