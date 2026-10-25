package runtimeplan

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
)

func TestHealthPBRProjectionFailureFallbackAndRecovery(t *testing.T) {
	source, err := config.Read("../../examples/health-pbr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	n := &memoryNetwork{}
	m := &observedManager{generation: 1}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	assertPath := func(primary bool) {
		t.Helper()
		got, backup, probe := false, false, false
		for _, route := range n.applied.Routes {
			if route.Table == 100 && route.Metric == 100 {
				got = true
			}
			if route.Table == 100 && route.Metric == 200 && route.Interface == "wan1" {
				backup = true
			}
			if route.Table == 254 && route.Destination == "198.51.100.10/32" && route.Interface == "wan0" {
				probe = true
			}
		}
		if got != primary || !backup || !probe || len(n.applied.PolicyRules) != 1 || n.applied.PolicyRules[0].Table != 100 || n.applied.PolicyRules[0].Source != "192.168.10.0/24" {
			t.Fatal("PBR projection or independent probe route changed", n.applied)
		}
		if r.config.revision != 1 {
			t.Fatal("health rewrote operator revision")
		}
	}
	assertPath(false)
	var cfg health.Config
	for _, res := range source.Resources {
		if res.Service != nil && res.Service.Health != nil {
			cfg = res.Service.HealthConfig()
		}
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	sequence := uint64(0)
	publish := func(phase string, passed, failed int, active bool) {
		t.Helper()
		sequence++
		clock, err = bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		ticks := uint64(clock.BoottimeNanoseconds)
		snapshot := health.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, BootID: clock.BootID, ObservedNanos: ticks, ExpiresNanos: ticks + uint64(cfg.Freshness()), Active: active, State: old.State{Phase: phase, ConsecutivePassed: passed, ConsecutiveFailed: failed, LastCheckedAt: time.Now()}}
		if err := r.Observe(ctx, update(t, r, "service.health-a", sequence, snapshot)); err != nil {
			t.Fatal(err)
		}
		reconcileConditions(t, r)
	}
	publish("Passing", 1, 0, false)
	assertPath(false)
	publish("Healthy", 2, 0, true)
	assertPath(true)
	publish("Failing", 0, 1, true)
	assertPath(true)
	publish("Unhealthy", 0, 3, false)
	assertPath(false)
	publish("Healthy", 2, 0, true)
	assertPath(true)
	grant := r.guardGrants["service.health-a"]
	grant.untilNanos = 1
	r.guardGrants["service.health-a"] = grant
	reconcileConditions(t, r)
	assertPath(false)
	// A control restart must reacquire health, retaining the independent probe
	// and backup route while withholding the stale primary.
	recovered := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	recovered.manager = m
	if err := recovered.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	r = recovered
	assertPath(false)
}

func TestHealthPBRMaxAgeDoesNotRenewOnProjectionOrTransportReplay(t *testing.T) {
	source, err := config.Read("../../examples/health-pbr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for i := range source.Resources {
		if source.Resources[i].ID == "primary" {
			if err := json.Unmarshal([]byte(`{"fact":"service.health-a.active","max_age":"200ms"}`), source.Resources[i].When); err != nil {
				t.Fatal(err)
			}
		}
	}
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	var cfg health.Config
	for _, res := range source.Resources {
		if res.Service != nil && res.Service.Name == "health-a" {
			cfg = res.Service.HealthConfig()
		}
	}
	snapshot := health.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(cfg.Freshness()), Active: true, State: old.State{Phase: old.PhaseHealthy, ConsecutivePassed: cfg.HealthyThreshold}}
	u := update(t, r, "service.health-a", 1, snapshot)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	hasPrimary := func() bool {
		for _, route := range n.applied.Routes {
			if route.Table == 100 && route.Metric == 100 {
				return true
			}
		}
		return false
	}
	if !hasPrimary() {
		t.Fatal("fresh sample did not activate primary")
	}
	time.Sleep(250 * time.Millisecond)
	u.Sequence++
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if hasPrimary() {
		t.Fatal("projection or replay renewed the original observation time")
	}
	clock, err = bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedNanos = uint64(clock.BoottimeNanoseconds)
	snapshot.ExpiresNanos = snapshot.ObservedNanos + uint64(cfg.Freshness())
	u = update(t, r, "service.health-a", 3, snapshot)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	if !hasPrimary() {
		t.Fatal("new probe observation did not recover primary")
	}
}
