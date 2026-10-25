package runtimeplan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
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
	facts, factErr := r.conditionFactsLocked(r.config, time.Now())
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

func TestGracefulVRRPRejectsChangedOrExpiredAdmissionBeforeRecording(t *testing.T) {
	for _, mode := range []string{"expired", "readiness-lost", "source-cutover", "election-changed", "producer-changed"} {
		t.Run(mode, func(t *testing.T) {
			source := activationSource()
			n := &memoryNetwork{}
			r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
			r.manager = &memoryManager{}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
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
			if err := r.Accept(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
				t.Fatal(err)
			}
			grant, err := r.GrantVRRPActivation(ctx, pending)
			if err != nil || grant.ID == 0 {
				t.Fatal(grant, err)
			}
			active := bootVRRPSnapshot(t, c)
			active.ActivationState = "Ready"
			active.ActivationStartedNanos = s.ActivationStartedNanos
			active.ActivationGrant = grant
			u := update(t, r, pending.Name, 2, active)
			switch mode {
			case "expired":
				active.ActivationGrant.ExpiresNanos = grant.ObservedNanos + 1
				r.mu.Lock()
				admission := r.vrrpAdmissions[u.Name]
				admission.grant = active.ActivationGrant
				r.vrrpAdmissions[u.Name] = admission
				r.mu.Unlock()
				u = update(t, r, u.Name, 2, active)
			case "readiness-lost":
				lost := lease4(time.Now().Add(time.Hour))
				lost.Active = false
				if err := r.Accept(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
					t.Fatal(err)
				}
			case "source-cutover":
				if err := r.ApplyConfig(ctx, 2, source); err != nil {
					t.Fatal(err)
				}
			case "election-changed":
				active.Transition++
				u = update(t, r, u.Name, 2, active)
			case "producer-changed":
				u.ProducerEpoch = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			if err := r.Accept(ctx, u); err == nil {
				t.Fatal("invalid admission accepted")
			}
			r.mu.Lock()
			remembered := r.records[u.Name]
			r.mu.Unlock()
			if remembered.Sequence != pending.Sequence || string(remembered.Snapshot) != string(pending.Snapshot) {
				t.Fatal("rejected admission changed the accepted election")
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if len(n.applied.Links) != 0 {
				t.Fatal("invalid admission installed a VIP")
			}
		})
	}
}

type activationPromotionNetwork struct {
	memoryNetwork
	failPromotion                     bool
	provisionalExpiry, originalExpiry uint64
}

func (n *activationPromotionNetwork) Apply(ctx context.Context, revision uint64, d network.Desired) error {
	for _, iface := range d.Interfaces {
		for address, opts := range iface.AddressOptions {
			if address != "192.0.2.254/24" {
				continue
			}
			if n.failPromotion && opts.BootLifetime.ValidNanos == n.originalExpiry {
				return errors.New("injected original-lifetime promotion failure")
			}
			if opts.BootLifetime.ValidNanos < n.originalExpiry {
				n.provisionalExpiry = opts.BootLifetime.ValidNanos
			}
		}
	}
	return n.memoryNetwork.Apply(ctx, revision, d)
}
func TestGracefulVRRPFailedPromotionCannotBecomeUnsolicitedLongOwnership(t *testing.T) {
	source := activationSource()
	n := &activationPromotionNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
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
	if err := r.Accept(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	grant, err := r.GrantVRRPActivation(ctx, pending)
	if err != nil || grant.ID == 0 {
		t.Fatal(grant, err)
	}
	active := bootVRRPSnapshot(t, c)
	active.ActivationState = "Ready"
	active.ActivationStartedNanos = s.ActivationStartedNanos
	active.ActivationGrant = grant
	n.failPromotion = true
	n.originalExpiry = active.ExpiresNanos
	u := update(t, r, pending.Name, 2, active)
	result := make(chan error, 1)
	go func() { result <- r.Accept(ctx, u) }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		accepted := r.records[u.Name].Sequence == u.Sequence
		r.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publication not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.reconcileDynamic(ctx); err == nil {
		t.Fatal("promotion failure hidden")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("failed promotion acknowledged")
		}
	case <-ctx.Done():
		t.Fatal("publisher not released")
	}
	if n.provisionalExpiry != grant.ExpiresNanos {
		t.Fatal("provisional VIP did not use original admission expiry", n.provisionalExpiry, grant.ExpiresNanos)
	}
	r.mu.Lock()
	_, latched := r.vrrpPublications[u.Name]
	_, stillGranted := r.vrrpAdmissions[u.Name]
	r.mu.Unlock()
	if latched || stillGranted {
		t.Fatal("failed promotion retained authority for later unsolicited publication")
	}
	n.failPromotion = false
	_ = r.reconcileDynamic(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.applied.Links) != 0 {
		t.Fatal("failed publication was later applied without a new admission")
	}
}

func TestGracefulSourceCutoverCannotApplyOldUnacknowledgedAdmission(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
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
	if err := r.Accept(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	grant, err := r.GrantVRRPActivation(ctx, pending)
	if err != nil || grant.ID == 0 {
		t.Fatal(grant, err)
	}
	active := bootVRRPSnapshot(t, c)
	active.ActivationState = "Ready"
	active.ActivationStartedNanos = s.ActivationStartedNanos
	active.ActivationGrant = grant
	u := update(t, r, pending.Name, 2, active)
	result := make(chan error, 1)
	go func() { result <- r.Accept(ctx, u) }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		accepted := r.records[u.Name].Sequence == u.Sequence
		r.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publication not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("old source permission acknowledged")
		}
	case <-ctx.Done():
		t.Fatal("old pending publication not fenced")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.applied.Links) != 0 {
		t.Fatal("source cutover applied stale pre-cutover VIP projection")
	}
}

func TestGracefulConditionalReactivationCannotReuseWithdrawnPublication(t *testing.T) {
	source := activationSource()
	n := &memoryNetwork{}
	m := &memoryManager{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
	if err := r.Accept(ctx, update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	grant, err := r.GrantVRRPActivation(ctx, pending)
	if err != nil || grant.ID == 0 {
		t.Fatal(grant, err)
	}
	active := bootVRRPSnapshot(t, c)
	active.ActivationState = "Ready"
	active.ActivationStartedNanos = s.ActivationStartedNanos
	active.ActivationGrant = grant
	if err := acceptVRRPAndDrain(t, r, update(t, r, pending.Name, 2, active)); err != nil {
		t.Fatal(err)
	}
	lost := lease4(time.Now().Add(time.Hour))
	lost.Active = false
	if err := r.Accept(ctx, update(t, r, "service.wan", 2, lost)); err != nil {
		t.Fatal(err)
	}
	source.Resources[0].When = &resourceprogram.FactCondition{Fact: "service.wan.active"}
	// The manager retires the worker without a final observation, as SIGKILL can.
	if err := r.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("conditional removal kept VIP")
	}
	if err := r.Accept(ctx, update(t, r, "service.wan", 3, lease4(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	m.vrrpGeneration = 2
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("unbound former role acquired VIP before new worker publication")
	}
	_ = r.reconcileDynamic(ctx)
	if len(n.applied.Links) != 0 {
		t.Fatal("new generation reused old publication")
	}
}
