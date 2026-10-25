//go:build linux

package vrrp

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

type activationEffects struct {
	device    *fakeTransport
	allowed   bool
	queries   int
	snapshots []Snapshot
}

func (e *activationEffects) Update(_ context.Context, s Snapshot) error {
	e.snapshots = append(e.snapshots, s)
	e.device.mu.Lock()
	e.device.active = s.Active
	e.device.mu.Unlock()
	return nil
}
func (e *activationEffects) GrantActivation(_ context.Context, s Snapshot) (ActivationGrant, error) {
	e.queries++
	if !e.allowed {
		return ActivationGrant{}, nil
	}
	return ActivationGrant{ID: 1, BootID: s.BootID, ElectionEpoch: s.Epoch, Transition: s.Transition, ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ObservedNanos + uint64(time.Second)}, nil
}

func TestGracefulTimeoutNeverPublishesAndReadinessIsAdmissionOnly(t *testing.T) {
	c := Normalize(Config{Resource: "ha", Interface: "lan0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, AddressOwner: true, Addresses: []string{"192.0.2.254/24"}, GracefulActivation: &GracefulActivation{Timeout: "1ns"}})
	c.GracefulActivation.ReadyWhen.Fact = "service.wan.active"
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	d := newFakeTransport()
	effects := &activationEffects{device: d}
	if err := applyActions(context.Background(), m, m.start(time.Now()), d, effects); err != nil {
		t.Fatal("denied admission must keep election alive:", err)
	}
	m.activationStartedNanos = 1 // elapsed BOOTTIME, without changing any system clock
	if err := applyActions(context.Background(), m, actions{update: true, advertise: true, notify: true}, d, effects); err != nil {
		t.Fatal(err)
	}
	if m.published || m.snapshot().ActivationState != "ReadinessTimeout" {
		t.Fatal("timeout force-published or was not reported", m.snapshot())
	}
	for _, s := range effects.snapshots {
		if s.Active {
			t.Fatal("unready role requested VIP ownership")
		}
	}
	for len(d.sent) > 0 {
		frame := <-d.sent
		if len(frame) < 34 || frame[23] != 112 {
			t.Fatal("unready role emitted neighbor announcement")
		}
	}
	effects.allowed = true
	if err := applyActions(context.Background(), m, actions{update: true}, d, effects); err != nil {
		t.Fatal(err)
	}
	if !m.published || m.snapshot().ActivationState != "Ready" {
		t.Fatal("late readiness did not publish retryable election", m.snapshot())
	}
	queries := effects.queries
	effects.allowed = false
	if err := applyActions(context.Background(), m, actions{update: true, advertise: true}, d, effects); err != nil {
		t.Fatal(err)
	}
	if !m.published || queries != effects.queries {
		t.Fatal("readiness loss revoked previously published election")
	}
}
