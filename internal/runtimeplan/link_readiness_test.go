//go:build linux

package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type delayedReadinessManager struct{ memoryManager }

func (m *delayedReadinessManager) Status(ctx context.Context) (supervisor.Status, error) {
	time.Sleep(1100 * time.Millisecond)
	return m.memoryManager.Status(ctx)
}

func TestReadinessUsesTheSupervisorsHeartbeatDeadline(t *testing.T) {
	r, _, manager := fixture(t, config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}}})
	if err := r.CheckReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.manager = &delayedReadinessManager{*manager}
	if err := r.CheckReady(context.Background()); err != nil {
		t.Fatal("fresh supervisor observation was rejected because the request waited", err)
	}
	r.manager = manager
	if err := r.CheckReady(context.Background()); err != nil {
		t.Fatal("fresh supervisor observation did not restore readiness", err)
	}
}

func TestReadinessRefreshDoesNotConfirmExpiredNetworkAuthority(t *testing.T) {
	for name, resource := range map[string]resourceprogram.Resource{
		"utc-route":   {Route: &network.Route{Destination: "2001:db8:42::/64", Interface: "wan0"}},
		"utc-address": {Address: &resourceprogram.Address{Interface: "wan0", CIDR: "2001:db8::42/64"}},
	} {
		t.Run(name, func(t *testing.T) {
			n := &memoryNetwork{}
			r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
			r.manager = &observedManager{generation: 1}
			resource.ID, resource.DependsOn = "owned", []string{"probe"}
			until := time.Now().Add(500 * time.Millisecond)
			if name == "utc-route" {
				resource.Route.ValidUntil = lifetime.Deadline{UTC: until}
			}
			if name == "utc-address" {
				resource.Address.Options = &network.AddressOptions{PreferredUntil: until, ValidUntil: until}
			}
			source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
				{ID: "probe", ReadyWhen: &resourceprogram.FactCondition{Fact: "service.probe.ready"}, Service: &serviceconfig.Service{Name: "probe", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}},
				resource,
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
			if !r.resourceStates["owned"].Active || r.applied.untilNanos == 0 {
				t.Fatalf("finite network grant was not installed before its cutoff %s: now=%s state=%+v", until, time.Now(), r.resourceStates["owned"])
			}
			before := n.calls
			time.Sleep(1100 * time.Millisecond)
			if err := r.CheckReady(ctx); err == nil {
				t.Fatal("new readiness observation confirmed expired network authority")
			}
			if err := r.reconcileDynamic(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.CheckReady(ctx); err != nil || n.calls <= before {
				t.Fatal("fresh authority was not applied before reporting readiness", err, n.calls, before)
			}
		})
	}
}
