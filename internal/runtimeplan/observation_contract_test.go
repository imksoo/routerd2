package runtimeplan

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestObservationReceiptDoesNotDependOnUnrelatedConditions(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		source := config.FromNative(wanDoc())
		if conditional {
			source.Resources = append(source.Resources, resourceprogram.Resource{ID: "unused", Interface: &network.Interface{Name: "unused0"}, When: &resourceprogram.FactCondition{Fact: "missing"}})
		}
		n := &memoryNetwork{}
		r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
		r.manager = &memoryManager{}
		ctx := context.Background()
		if err := r.ApplyConfig(ctx, 1, source); err != nil {
			t.Fatal(err)
		}
		calls := n.calls
		if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
			t.Fatal(err)
		}
		r.reportReadiness(ctx)
		if status := r.Health(); status.Phase != "applying" || status.LastError != "" {
			t.Fatal("ordinary observation/application progress reported a failure", status)
		}
		if n.calls != calls || r.CheckReady(ctx) == nil {
			t.Fatal("observation receipt performed kernel IO or advertised unapplied readiness", conditional)
		}
		if err := r.reconcileDynamic(ctx); err != nil || len(n.applied.Routes) != 1 || r.CheckReady(ctx) != nil {
			t.Fatal("coordinator did not apply the observation", conditional, err)
		}
	}
}

func TestObservationProgressDuringNetworkIORequiresOnlyChangedEffects(t *testing.T) {
	for _, change := range []string{"unchanged", "extended", "shortened"} {
		t.Run(change, func(t *testing.T) {
			r, native, _ := fixture(t, wanDoc())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			first := lease4(time.Now().Add(time.Hour))
			if err := r.Observe(ctx, update(t, r, "service.wan", 1, first)); err != nil {
				t.Fatal(err)
			}
			blocked, release := blockBFDNetwork(t, r, native, nil)
			done := make(chan error, 1)
			go func() { done <- r.reconcileDynamic(ctx) }()
			select {
			case <-blocked.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			newer := first
			switch change {
			case "extended":
				newer = lease4(time.Now().Add(2 * time.Hour))
			case "shortened":
				newer = lease4(time.Now().Add(30 * time.Minute))
			}
			observed := make(chan error, 1)
			publication := update(t, r, "service.wan", 2, newer)
			go func() { observed <- r.Observe(ctx, publication) }()
			select {
			case err := <-observed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("network IO blocked observation acceptance")
			}
			if r.CheckReady(ctx) == nil {
				t.Fatal("in-flight network transaction reported ready")
			}
			if page, err := r.resourceObservation(0, ""); err != nil || page.Phase == "ready" {
				t.Fatal("resource view advertised in-flight projection as ready", page.Phase, err)
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := r.CheckReady(ctx); (err != nil) != (change == "shortened") {
				t.Fatal("readiness did not distinguish metadata from changed effects", err)
			}
			if err := r.reconcileDynamic(ctx); err != nil || r.CheckReady(ctx) != nil {
				t.Fatal("newer observation did not converge", err)
			}
			if native.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].BootLifetime.ValidNanos != newer.LeaseClock.ExpiresNanos {
				t.Fatal("newer lease was lost or its original deadline changed")
			}
		})
	}
}

func TestPrerequisitesAreRefreshedAfterKernelIO(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(strconv.FormatBool(live), func(t *testing.T) {
			r, n, m := conditionalFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			lease := lease4(time.Now().Add(time.Hour))
			if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease)); err != nil {
				t.Fatal(err)
			}
			blocked, release := blockBFDNetwork(t, r, n, nil)
			done := make(chan error, 1)
			go func() { done <- r.reconcileDynamic(ctx) }()
			select {
			case <-blocked.entered:
			case <-ctx.Done():
				t.Fatal("application did not reach the kernel writer")
			}
			down := lease4(time.Now().Add(time.Hour))
			down.Active = live
			if err := r.Observe(ctx, update(t, r, "service.wan", 2, down)); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			r.guard.untilNanos = 1
			r.mu.Unlock()
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := 1
			if live {
				want = 2
			}
			if len(m.modules) != want {
				t.Fatal("worker set does not match the fresh supervisor and protocol observation", live, m.modules)
			}
			if err := r.reconcileDynamic(ctx); err != nil {
				t.Fatal(err)
			}
			if conditionalRoute(n) != live || len(m.modules) != want {
				t.Fatal("latest withdrawal did not converge")
			}

		})
	}
}

func TestConfigurationStopReportsApplyingBeforeProcessExit(t *testing.T) {
	r, _, manager := fixture(t, wanDoc())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	manager.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 0 {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { done <- r.ApplyConfig(ctx, 2, config.FromNative(config.Native{})) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("configuration did not request process stop")
	}
	if r.Health().Phase == "ready" {
		t.Fatal("configuration remained Ready during its stop request")
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("configuration stop did not finish")
	}
}

func TestConfigurationCanChangePrerequisitesOfAnUnreadyRetainedWorker(t *testing.T) {
	r, n, manager := conditionalFixture(t)
	manager.down = true
	source := conditionSource()
	source.Resources[3].Interface.MTU = 1500
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := r.ApplyConfig(ctx, 2, source); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configuration should await worker readiness after applying: %v", err)
	}
	for _, iface := range n.applied.Interfaces {
		if iface.Name == "other0" && iface.MTU == 1500 {
			return
		}
	}
	t.Fatal("unready retained worker prevented the new network configuration from being applied")
}
