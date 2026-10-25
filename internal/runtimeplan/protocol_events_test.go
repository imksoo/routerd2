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

func TestRetirementGuardRequiresSourceRemovalAndNoRemainingHealthWorker(t *testing.T) {
	ctx := context.Background()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	m := &observedManager{generation: 1}
	r.manager = m
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}}}}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	observe := func() (allowed map[string]bool, err error) {
		err = r.WithStoppedHealthRelays(ctx, []string{"wan"}, func(v map[string]bool) error { allowed = v; return nil })
		return
	}
	allowed, err := observe()
	if err != nil || allowed["wan"] {
		t.Fatal("declared health relay collected", allowed, err)
	}
	oldModules := append([]supervisor.DynamicModule(nil), m.modules...)
	if err = r.ApplyConfig(ctx, 2, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	m.modules = oldModules // supervisor still reports the old process while stopping
	allowed, err = observe()
	if err != nil || allowed["wan"] {
		t.Fatal("stopping health worker was bypassed", allowed, err)
	}
	m.modules = nil
	allowed, err = observe()
	if err != nil || !allowed["wan"] {
		t.Fatal(allowed, err)
	}
	if len(r.records) != 0 {
		t.Fatal("history recovery registered current protocol facts")
	}
	m.unavailable = true
	if _, err = observe(); err == nil {
		t.Fatal("unknown supervisor granted recovery")
	}
}
