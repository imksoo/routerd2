package runtimeplan

import (
	"context"
	"errors"
	"testing"
	"time"

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
	r.op.Lock()
	deadline, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	err := r.WithStoppedHealthRelays(deadline, []string{"wan"}, func(map[string]bool) error {
		t.Fatal("owned reconciliation was bypassed")
		return nil
	})
	cancel()
	r.op.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("busy recovery silently skipped its ownership observation", err)
	}
	m.unavailable = true
	if err := r.WithStoppedHealthRelays(ctx, []string{"wan"}, func(map[string]bool) error {
		t.Fatal("unknown supervisor granted disk ownership")
		return nil
	}); err == nil {
		t.Fatal("missing supervisor was hidden")
	}
}

func TestStoppedHealthRecoveryWaitsForReconcileAndThenObservesOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	if err := r.ApplyConfig(ctx, 1, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	r.op.Lock()
	done := make(chan error, 1)
	called := false
	go func() {
		done <- r.WithStoppedHealthRelays(ctx, []string{"wan"}, func(allowed map[string]bool) error {
			called = allowed["wan"]
			return nil
		})
	}()
	select {
	case err := <-done:
		r.op.Unlock()
		t.Fatal("contention silently discarded the recovery pass", err)
	case <-time.After(25 * time.Millisecond):
	}
	r.op.Unlock()
	select {
	case err := <-done:
		if err != nil || !called {
			t.Fatal("recovery did not acquire ownership after reconcile completed", called, err)
		}
	case <-ctx.Done():
		t.Fatal("recovery starved behind reconcile")
	}
}
