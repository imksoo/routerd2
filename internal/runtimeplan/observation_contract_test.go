package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
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
		if n.calls != calls || r.CheckReady(ctx) == nil {
			t.Fatal("observation receipt performed kernel IO or advertised unapplied readiness", conditional)
		}
		if err := r.reconcileDynamic(ctx); err != nil || len(n.applied.Routes) != 1 || r.CheckReady(ctx) != nil {
			t.Fatal("coordinator did not apply the observation", conditional, err)
		}
	}
}

func TestObservationProgressDuringNetworkIOCannotAcknowledgeNewerInput(t *testing.T) {
	r, native, _ := fixture(t, wanDoc())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
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
	newer := lease4(time.Now().Add(2 * time.Hour))
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
	if r.CheckReady(ctx) == nil {
		t.Fatal("older transaction acknowledged newer input")
	}
	if err := r.reconcileDynamic(ctx); err != nil || r.CheckReady(ctx) != nil {
		t.Fatal("newer observation did not converge", err)
	}
	if native.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].BootLifetime.ValidNanos != newer.LeaseClock.ExpiresNanos {
		t.Fatal("newer lease was lost or its original deadline changed")
	}
}
