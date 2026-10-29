package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestConditionHoldTimerAndFlapBetweenProjections(t *testing.T) {
	ctx := context.Background()
	source := conditionSource()
	if err := json.Unmarshal([]byte(`{"fact":"service.wan.active","for":"200ms","max_age":"1s"}`), source.Resources[1].When); err != nil {
		t.Fatal(err)
	}
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	lease := lease4(time.Now().Add(time.Hour))
	sequence := uint64(0)
	publish := func(active bool) {
		t.Helper()
		sequence++
		lease = lease4(time.Now().Add(time.Hour))
		lease.Active = active
		if err := r.Observe(ctx, update(t, r, "service.wan", sequence, lease)); err != nil {
			t.Fatal(err)
		}
	}
	publish(true)
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("route activated before hold time")
	}
	time.Sleep(250 * time.Millisecond)
	reconcileConditions(t, r) // timer, without a new observation
	if !conditionalRoute(n) {
		t.Fatal("hold never released without a new protocol message")
	}
	// Both callbacks arrive before the next projection. The false sample must
	// break continuity even though the projected snapshots would both be true.
	publish(false)
	publish(true)
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("flap between projections did not reset hold")
	}
	time.Sleep(250 * time.Millisecond)
	reconcileConditions(t, r)
	if !conditionalRoute(n) {
		t.Fatal("continuous recovery failed after flap")
	}
}

type observedManager struct {
	memoryManager
	generation        uint64
	down, unavailable bool
}

func (m *observedManager) Status(ctx context.Context) (supervisor.Status, error) {
	if m.unavailable {
		return supervisor.Status{}, errors.New("supervisor unavailable")
	}
	s, err := m.memoryManager.Status(ctx)
	for i := range s.Modules {
		s.Modules[i].Generation = m.generation
		if s.Modules[i].Name == "service.wan" && m.down {
			s.Modules[i].Ready = false
			s.Modules[i].State = "backoff"
		}
	}
	return s, err
}
func conditionSource() config.Document {
	return config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "wan", ReadyWhen: &resourceprogram.FactCondition{Fact: "service.wan.ready"}, Service: &serviceconfig.Service{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}}},
		{ID: "route", Owner: "wan", When: &resourceprogram.FactCondition{Fact: "service.wan.active"}, Route: &network.Route{Destination: "198.51.100.0/24", Interface: "wan0", Gateway: "192.0.2.1"}},
		{ID: "dependent", DependsOn: []string{"route"}, Service: &serviceconfig.Service{Name: "dependent", DHCP4: &serviceconfig.DHCP4{Interface: "lan0"}}},
		{ID: "other", Interface: &network.Interface{Name: "other0", MTU: 1400}},
	}}
}
func conditionalRoute(n *memoryNetwork) bool {
	for _, route := range n.applied.Routes {
		if route.Destination == "198.51.100.0/24" {
			return true
		}
	}
	return false
}
func conditionalFixture(t *testing.T) (*Runtime, *memoryNetwork, *observedManager) {
	t.Helper()
	n := &memoryNetwork{}
	m := &observedManager{generation: 1}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	if err := r.ApplyConfig(context.Background(), 1, conditionSource()); err != nil {
		t.Fatal(err)
	}
	return r, n, m
}
func reconcileConditions(t *testing.T, r *Runtime) {
	t.Helper()
	// The in-memory supervisor reaps immediately. Give the owner a fresh
	// observation after its stop request, as the real polling loop does.
	for range 2 {
		if err := r.reconcileDynamic(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestConditionalRuntimeStartWithdrawRecover(t *testing.T) {
	ctx := context.Background()
	r, n, m := conditionalFixture(t)
	if conditionalRoute(n) || len(m.modules) != 1 {
		t.Fatal("unknown condition started dependent work")
	}
	up := update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))
	if err := r.Observe(ctx, up); err != nil {
		t.Fatal(err)
	}
	if conditionalRoute(n) {
		t.Fatal("callback bypassed serialized worker reconciliation")
	}
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 2 && !conditionalRoute(n) {
			t.Fatal("dependent started before network ACK")
		}
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) || len(m.modules) != 2 {
		t.Fatal("ready live fact failed to start dependents", n.applied, m.modules)
	}
	facts, err := r.conditionFactsLocked(r.config)
	if err != nil || facts.Facts["service.wan.active"].ExpiresNanos != r.guardGrants[up.Name].untilNanos {
		t.Fatal("supervisor poll shortened the acquired lease", facts, err)
	}
	// Crash of the producing worker revokes the route and dependent worker.
	stoppedBeforeWithdrawal := false
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 1 {
			stoppedBeforeWithdrawal = stoppedBeforeWithdrawal || conditionalRoute(n)
		}
	}
	m.down = true
	reconcileConditions(t, r)
	if conditionalRoute(n) || len(m.modules) != 1 || !stoppedBeforeWithdrawal {
		t.Fatal("unsafe withdrawal order", n.applied, m.modules)
	}
	if len(r.config.source.Resources) != 4 || r.config.revision != 1 {
		t.Fatal("dynamic change rewrote source or operator revision")
	}
	// New generation cannot revive the old Up; it needs a new accepted report.
	m.down = false
	m.generation = 2
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("old generation revived")
	}
	up = update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))
	up.Generation = 2
	if err := r.Observe(ctx, up); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) || len(m.modules) != 2 {
		t.Fatal("new producer did not recover")
	}
}
func TestConditionalFactExpiryReplayAndRevisionFence(t *testing.T) {
	ctx := context.Background()
	r, n, _ := conditionalFixture(t)
	u := update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) {
		t.Fatal("missing route")
	}
	grant := r.guardGrants[u.Name]
	grant.untilNanos = 1
	r.guardGrants[u.Name] = grant
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	} // duplicate must not renew
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("duplicate renewed expired fact")
	}
	u.Sequence++
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	} // same deadline, larger transport sequence
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("transport sequence renewed same expired grant")
	}
	u = update(t, r, "service.wan", 3, lease4(time.Now().Add(2*time.Hour)))
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) {
		t.Fatal("fresh lease did not recover")
	}
	replacement := conditionSource()
	replacement.Resources[0].Service.DHCP4.Interface = "wan1"
	if err := r.ApplyConfig(ctx, 2, replacement); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("replaced DHCP producer retained its old route")
	}
}

func TestConditionalLeaseObservationDoesNotRefreshOnTransportReplay(t *testing.T) {
	ctx := context.Background()
	r, _, _ := conditionalFixture(t)
	lease := lease4(time.Now().Add(time.Hour))
	u := update(t, r, "service.wan", 1, lease)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	original := r.guardGrants[u.Name]
	u.Sequence++
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	replayed := r.guardGrants[u.Name]
	if replayed.observedNanos != original.observedNanos || replayed.untilNanos != original.untilNanos {
		t.Fatal("same lease acquired a new observation or expiration on replay")
	}
	lease = lease4(time.Now().Add(2 * time.Hour))
	u = update(t, r, "service.wan", 3, lease)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	renewed := r.guardGrants[u.Name]
	if renewed.observedNanos <= original.observedNanos || renewed.untilNanos <= original.untilNanos {
		t.Fatal("new lease failed to renew observation and expiration")
	}
	clock, _ := boottime.Read()
	lease.ObservedNanos = uint64(clock.BoottimeNanoseconds)
	lease.Sequence = lease.ObservedNanos
	lease.UpdatedAt = clock.WallTime
	lease.Active = false
	u = update(t, r, "service.wan", 4, lease)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	inactive := r.guardGrants[u.Name]
	u.Sequence++
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	if replayed := r.guardGrants[u.Name]; replayed.observedNanos != inactive.observedNanos || replayed.untilNanos != inactive.untilNanos {
		t.Fatal("identical inactive lease renewed its synthetic finite grant")
	}
}
func TestConditionalNetworkFailureDoesNotStartWorker(t *testing.T) {
	ctx := context.Background()
	r, n, m := conditionalFixture(t)
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	n.fail = errors.New("injected network failure")
	if err := r.reconcileDynamic(ctx); err == nil {
		t.Fatal("network failure hidden")
	}
	if len(m.modules) != 1 || conditionalRoute(n) {
		t.Fatal("worker started without ACK")
	}
	reconcileConditions(t, r)
	if len(m.modules) != 2 || !conditionalRoute(n) {
		t.Fatal("retry did not recover")
	}
}
func TestConditionalUnavailableSupervisorWithdraws(t *testing.T) {
	ctx := context.Background()
	r, n, m := conditionalFixture(t)
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	m.unavailable = true
	// We cannot claim stopped workers when supervisor is unreachable. The call
	// must fail rather than withdrawing the network beneath a live dependent.
	if err := r.reconcileDynamic(ctx); err == nil {
		t.Fatal("unconfirmed quiescence accepted")
	}
	if !conditionalRoute(n) {
		t.Fatal("network withdrawn before confirmed stop")
	}
	m.unavailable = false
	m.down = true
	reconcileConditions(t, r)
	if conditionalRoute(n) {
		t.Fatal("did not withdraw after supervisor recovery")
	}
}

func TestConditionalChangedFactDuringStopRestartsQuiescedWorker(t *testing.T) {
	ctx := context.Background()
	r, n, m := conditionalFixture(t)
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	grant := r.guardGrants["service.wan"]
	grant.value = false
	r.guardGrants["service.wan"] = grant
	changed := false
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 1 && !changed {
			changed = true
			grant := r.guardGrants["service.wan"]
			grant.value = true
			r.guardGrants["service.wan"] = grant
		}
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	} // stop request only; observe exit before applying
	if len(m.modules) != 1 {
		t.Fatal("test did not quiesce dependent")
	}
	m.before = nil
	reconcileConditions(t, r)
	if len(m.modules) != 2 || !conditionalRoute(n) {
		t.Fatal("matching old binding masked a stopped worker", m.modules, n.applied)
	}
}

func TestConditionalUnchangedProducerSurvivesSourceDigestCutover(t *testing.T) {
	ctx := context.Background()
	r, n, _ := conditionalFixture(t)
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) {
		t.Fatal("missing initial route")
	}
	source := conditionSource()
	source.Resources[3].Interface.MTU = 1450
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !conditionalRoute(n) {
		t.Fatal("unrelated MTU change discarded the current DHCP lease")
	}
}

func (r *Runtime) refreshGuardStatus(ctx context.Context) {
	status, err := r.manager.Status(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installGuardStatusLocked(status, err)
}
