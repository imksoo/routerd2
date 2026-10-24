package runtimeplan

import (
	"context"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestStoppedHealthRecoveryFencesReactivationWithoutHoldingFactMutex(t *testing.T) {
	ctx := context.Background()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	m := &observedManager{generation: 1}
	r.manager = m
	service := func(name string) *serviceconfig.Service {
		return &serviceconfig.Service{Name: name, Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}
	}
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "switch", Service: service("switch")},
		{ID: "wan", Service: service("wan"), When: &resourceprogram.FactCondition{Fact: "service.switch.active"}},
	}}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		called := false
		err := r.WithStoppedHealthRelays(ctx, []string{"switch", "wan"}, func(allowed map[string]bool) error {
			called = true
			if allowed["switch"] || allowed["wan"] != want {
				t.Fatal("active ownership or inactive eligibility lost", allowed)
			}
			if r.op.TryLock() {
				r.op.Unlock()
				t.Fatal("conditional worker reactivation can race disk recovery")
			}
			if !r.mu.TryLock() {
				t.Fatal("historical disk IO blocks current fact acceptance")
			}
			r.mu.Unlock()
			_ = r.Health() // bounded current views remain available during IO
			return nil
		})
		if err != nil || !called {
			t.Fatal(called, err)
		}
	}
	check(true) // unknown predicate has not created wan; declaration remains
	m.modules = append(m.modules, supervisor.DynamicModule{Name: "service.wan", Role: "health"})
	check(false) // an exiting worker independently forbids collection
	m.modules = m.modules[:1]
	// Even an incomplete supervisor reply cannot override a selected binding.
	r.mu.Lock()
	r.bindings["service.wan"] = r.bindings["service.switch"]
	r.mu.Unlock()
	check(false)
	m.unavailable = true
	if err := r.WithStoppedHealthRelays(ctx, []string{"wan"}, func(map[string]bool) error {
		t.Fatal("unknown supervisor granted disk ownership")
		return nil
	}); err == nil {
		t.Fatal("missing supervisor was hidden")
	}
}
