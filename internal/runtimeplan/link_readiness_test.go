//go:build linux

package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestReadinessRefreshDoesNotChangePublicStateOrConfirmExpiredLinkAuthority(t *testing.T) {
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "probe", ReadyWhen: &resourceprogram.FactCondition{Fact: "service.probe.ready"}, Service: &serviceconfig.Service{Name: "probe", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}},
		{ID: "identity", DependsOn: []string{"probe"}, Link: &network.Link{Name: "ha", Kind: "bridge"}},
	}}
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := r.CheckReady(ctx); err != nil {
			t.Fatal("fresh supervisor timestamps prevented convergence", err)
		}
	}
	before := n.calls
	time.Sleep(1100 * time.Millisecond)
	if err := r.CheckReady(ctx); err == nil {
		t.Fatal("new readiness observation confirmed expired applied link authority")
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckReady(ctx); err != nil || n.calls <= before {
		t.Fatal("fresh authority was not applied before reporting readiness", err, n.calls, before)
	}
}
