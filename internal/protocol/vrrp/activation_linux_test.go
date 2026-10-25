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
	published bool
	snapshots []Snapshot
}

func (e *activationEffects) Apply(_ context.Context, s Snapshot) (Decision, error) {
	e.snapshots = append(e.snapshots, s)
	e.device.mu.Lock()
	e.device.active = e.published
	e.device.mu.Unlock()
	return Decision{Active: e.published}, nil
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
	effects.published = true
	if err := applyActions(context.Background(), m, actions{update: true}, d, effects); err != nil {
		t.Fatal(err)
	}
	if !m.published || m.snapshot().ActivationState != "Ready" {
		t.Fatal("late readiness did not publish retryable election", m.snapshot())
	}
}
