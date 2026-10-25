package runtimeplan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
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
func acceptVRRPAndDrain(t *testing.T, r *Runtime, u serviceconfig.Update) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Accept(ctx, u) }()
	for {
		select {
		case err := <-result:
			return err
		default:
		}
		if err := r.reconcileDynamic(ctx); err != nil && !errors.Is(err, ErrVRRPActivationPending) {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			t.Fatal("effect not completed")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestGracefulVRRPRequiresCurrentAdmissionAndLatchesOnlyAppliedPublication(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx := context.Background()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	c := source.Resources[0].Service.VRRPConfig()
	s := bootVRRPSnapshot(t, c)
	s.Active = false
	s.ValidUntil = time.Time{}
	s.ActivationState = "Preparing"
	s.ActivationStartedNanos = s.ObservedNanos
	pending := update(t, r, "service.ha", 1, s)
	if err := acceptVRRPAndDrain(t, r, pending); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckReady(ctx); err == nil {
		t.Fatal("unpublished elected MASTER was reported healthy")
	}
	r.mu.Lock()
	facts, factErr := r.conditionFactsLocked(r.source, r.revision, time.Now())
	r.mu.Unlock()
	if factErr != nil || facts.Value("service.ha.elected") != resourceprogram.True || facts.Value("service.ha.active") != resourceprogram.False {
		t.Fatal("election and publication facts were conflated", factErr)
	}
	grant, err := r.GrantVRRPActivation(ctx, pending)
	if err != nil || grant.ID != 0 || len(n.applied.Links) != 0 {
		t.Fatal("unready elected MASTER acquired VIP", grant, err)
	}
	// A forged Active request must not bypass the control-side admission gate.
	forged := s
	forged.Active = true
	forged.ActivationState = "Ready"
	forged.ValidUntil = time.Now().Add(time.Hour)
	if err := r.Accept(ctx, update(t, r, pending.Name, 2, forged)); err == nil {
		t.Fatal("VIP request without a current admission grant accepted")
	}
	if err := r.Accept(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	grant, err = r.GrantVRRPActivation(ctx, pending)
	if err != nil || grant.ID == 0 {
		t.Fatal("current readiness did not grant admission", grant, err)
	}
	active := bootVRRPSnapshot(t, c)
	active.ActivationState = "Ready"
	active.ActivationStartedNanos = s.ActivationStartedNanos
	active.ActivationGrant = grant
	if err := acceptVRRPAndDrain(t, r, update(t, r, pending.Name, 2, active)); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) == 0 {
		t.Fatal("admitted VIP not applied")
	}
	// Readiness is admission, not a standing permission to keep the MASTER VIP.
	lost := lease4(time.Now().Add(time.Hour))
	lost.Active = false
	if err := r.Accept(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) == 0 {
		t.Fatal("readiness degradation withdrew a published MASTER VIP")
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	renewed := active
	renewed.ObservedNanos = uint64(clock.BoottimeNanoseconds)
	renewed.ExpiresNanos = renewed.ObservedNanos + uint64(vrrp.OwnershipLifetime(c))
	if err := acceptVRRPAndDrain(t, r, update(t, r, pending.Name, 3, renewed)); err != nil {
		t.Fatal("published role could not renew after readiness degradation:", err)
	}
}
