package runtimeplan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func activationSource() config.Document {
	source := conditionalVRRPSource()
	source.Resources = source.Resources[:1]
	source.Resources[0].Service.VRRP.GracefulActivation = &vrrp.GracefulActivation{ReadyWhen: resourceprogram.FactCondition{Fact: "service.wan.active"}, Timeout: "1ms"}
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "wan", Service: &serviceconfig.Service{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}}})
	return source
}

func applyAndDrain(t *testing.T, r *Runtime, u serviceconfig.Update) (serviceconfig.ApplicationReceipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type outcome struct {
		receipt serviceconfig.ApplicationReceipt
		err     error
	}
	result := make(chan outcome, 1)
	go func() { receipt, err := r.Apply(ctx, u); result <- outcome{receipt, err} }()
	for {
		select {
		case v := <-result:
			return v.receipt, v.err
		default:
		}
		// A component test explicitly drives the production coordinator.
		if err := r.reconcileDynamic(ctx); err != nil {
			return serviceconfig.ApplicationReceipt{}, err
		}
		select {
		case v := <-result:
			return v.receipt, v.err
		case <-ctx.Done():
			t.Fatal("application did not complete")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestGracefulVRRPAdmissionReturnsAppliedOwnershipAndSurvivesReadinessLoss(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	if r.Health().Phase == "ready" {
		t.Fatal("startup reported Ready without an applied VRRP election")
	}
	c := source.Resources[0].Service.VRRPConfig()
	election := bootVRRPSnapshot(t, c)
	election.Active = false
	election.ValidUntil = time.Time{}
	election.ActivationState = "Preparing"
	election.ActivationStartedNanos = election.ObservedNanos
	u := update(t, r, "service.ha", 1, election)
	receipt, err := applyAndDrain(t, r, u)
	if err != nil || !receipt.Applied || receipt.VRRP.Active || len(n.applied.Links) != 0 || r.CheckReady(ctx) == nil {
		t.Fatal("unready election acquired ownership or reported healthy", receipt, err)
	}
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	u.Sequence++
	receipt, err = applyAndDrain(t, r, u)
	if err != nil || !receipt.VRRP.Active || len(n.applied.Links) != 1 || r.CheckReady(ctx) != nil {
		t.Fatal("readiness did not apply the elected VIP", receipt, err)
	}
	found := false
	for _, iface := range n.applied.Interfaces {
		if iface.Name == c.VIPInterface {
			found = iface.AddressOptions[c.Addresses[0]].BootLifetime.ValidNanos == election.ExpiresNanos
		}
	}
	if !found {
		t.Fatal("activation changed the election's original deadline")
	}
	lost := lease4(time.Now().Add(time.Hour))
	lost.Active = false
	if err := r.Observe(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) != 1 {
		t.Fatal("readiness loss revoked an admitted election")
	}
	r.mu.Lock()
	facts, factErr := r.conditionFactsLocked(r.config, time.Now())
	r.mu.Unlock()
	if factErr != nil || facts.Value("service.ha.active") != resourceprogram.True {
		t.Fatal("applied ownership was hidden by the worker's earlier pending observation", factErr)
	}
}

func TestGracefulVRRPFailedApplyRequiresCurrentReadinessOnRetry(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	election := bootVRRPSnapshot(t, source.Resources[0].Service.VRRPConfig())
	election.Active = false
	election.ValidUntil = time.Time{}
	election.ActivationState = "Preparing"
	election.ActivationStartedNanos = election.ObservedNanos
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	n.fail = errors.New("kernel apply failed")
	if err := r.Observe(ctx, update(t, r, "service.ha", 1, election)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err == nil {
		t.Fatal("kernel failure hidden")
	}
	lost := lease4(time.Now().Add(time.Hour))
	lost.Active = false
	if err := r.Observe(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("failed activation latched admission and later acquired the VIP")
	}
}

func TestGracefulVRRPAdmissionIsConsumedWhenKernelApplicationBegins(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	election := bootVRRPSnapshot(t, source.Resources[0].Service.VRRPConfig())
	election.Active = false
	election.ValidUntil = time.Time{}
	election.ActivationState = "Preparing"
	election.ActivationStartedNanos = election.ObservedNanos
	u := update(t, r, "service.ha", 1, election)
	if err := r.Observe(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := r.Observe(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	blocked, release := blockBFDNetwork(t, r, n, nil)
	monitorDone := make(chan struct{})
	go func() { r.Monitor(ctx); close(monitorDone) }()
	defer func() { cancel(); <-monitorDone }()
	type result struct {
		receipt serviceconfig.ApplicationReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() { receipt, err := r.Apply(ctx, u); done <- result{receipt, err} }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("admitted application did not reach the kernel writer")
	}
	lost := lease4(time.Now().Add(time.Hour))
	lost.Active = false
	if err := r.Observe(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case v := <-done:
		if v.err != nil || !v.receipt.Applied || !v.receipt.VRRP.Active {
			t.Fatal("readiness loss during admitted IO revoked successful ownership", v)
		}
	case <-ctx.Done():
		t.Fatal("successful application did not complete")
	}
	cancel()
	<-monitorDone
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.applied.Links) != 1 {
		t.Fatal("coordinator withdrew the admitted VIP after readiness loss")
	}
}
