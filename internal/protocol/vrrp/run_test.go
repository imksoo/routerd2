//go:build linux

package vrrp

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTransport struct {
	received chan []byte
	sent     chan []byte
	mu       sync.Mutex
	active   bool
	verify   error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{received: make(chan []byte, 16), sent: make(chan []byte, 1024)}
}
func (f *fakeTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-f.received:
		return frame, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeTransport) Send(ctx context.Context, frame []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f.sent <- append([]byte(nil), frame...):
		return nil
	}
}
func (f *fakeTransport) VerifyActive(_ context.Context, s Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verify != nil {
		return f.verify
	}
	if !f.active || !s.Active || !time.Now().Before(s.ValidUntil) {
		return errors.New("VIP not owned")
	}
	return nil
}
func (*fakeTransport) Close() error { return nil }

func TestRuntimeWaitsForOwnershipAndWithdrawsGracefully(t *testing.T) {
	c := config4()
	c.AddressOwner, c.Priority = true, 255
	device := newFakeTransport()
	entered, acknowledge, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var updates []Snapshot
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		mu.Lock()
		updates = append(updates, s)
		first := len(updates) == 1
		mu.Unlock()
		if first {
			close(entered)
			select {
			case <-acknowledge:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		device.mu.Lock()
		device.active = s.Active
		device.mu.Unlock()
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("a", 32), device, effects, func() { close(ready) })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ownership request was not made")
	}
	select {
	case <-device.sent:
		t.Fatal("advertisement or GARP preceded ownership acknowledgment")
	case <-ready:
		t.Fatal("worker ready before initial ownership settled")
	default:
	}
	close(acknowledge)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("worker did not become ready after ownership acknowledgment")
	}
	first := <-device.sent
	packet, err := parseFrame(c, first)
	if err != nil || packet.priority != 255 {
		t.Fatalf("first packet was not the active advertisement: %v", err)
	}
	<-device.sent // Gratuitous ARP follows the initial advertisement.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not shut down")
	}
	last := <-device.sent
	packet, err = parseFrame(c, last)
	if err != nil || packet.priority != 0 {
		t.Fatal("graceful shutdown omitted priority zero")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 2 || !updates[0].Active || updates[0].ValidUntil.IsZero() || updates[1].Active || updates[1].State != StateInitialize || !updates[1].ValidUntil.IsZero() || updates[1].Transition <= updates[0].Transition {
		t.Fatalf("ownership was not fenced and withdrawn: %+v", updates)
	}
}

func TestFailedOrExpiredActivationNeverAdvertises(t *testing.T) {
	for _, mode := range []string{"apply", "verify", "expired acknowledgement"} {
		t.Run(mode, func(t *testing.T) {
			c := config4()
			c.AddressOwner, c.Priority = true, 255
			if mode == "expired acknowledgement" {
				c.AdvertIntervalCentiseconds = 1
			}
			device := newFakeTransport()
			if mode == "verify" {
				device.verify = errors.New("configured VIP is still tentative")
			}
			withdrawn := false
			effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
				if !s.Active {
					withdrawn = true
					return nil
				}
				device.active = true
				switch mode {
				case "apply":
					return errors.New("partial network apply failed")
				case "expired acknowledgement":
					<-ctx.Done()
					return nil // A buggy effect acknowledges after its lease.
				}
				return nil
			})
			err := run(context.Background(), c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("b", 32), device, effects, func() { t.Error("failed activation became ready") })
			if err == nil || !withdrawn || len(device.sent) != 0 {
				t.Fatalf("failed activation advertised or retained ownership: mode=%s, sent=%d, withdrawn=%t, error=%v", mode, len(device.sent), withdrawn, err)
			}
		})
	}
}

func TestActiveAdvertisementsRenewAcknowledgedLease(t *testing.T) {
	c := config4()
	c.AddressOwner, c.Priority, c.AdvertIntervalCentiseconds = true, 255, 5
	device := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var leases []time.Time
	effects := EffectsFunc(func(_ context.Context, s Snapshot) error {
		device.mu.Lock()
		device.active = s.Active
		device.mu.Unlock()
		if s.Active {
			mu.Lock()
			leases = append(leases, s.ValidUntil)
			mu.Unlock()
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, c, netip.MustParseAddr(c.SourceAddress), strings.Repeat("c", 32), device, effects, nil)
	}()
	positive := 0
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for positive < 3 {
		select {
		case frame := <-device.sent:
			if packet, err := parseFrame(c, frame); err == nil && packet.priority != 0 {
				positive++
				mu.Lock()
				if len(leases) < positive {
					t.Error("positive advertisement preceded its lease renewal")
				}
				mu.Unlock()
			}
		case <-deadline.C:
			t.Fatal("periodic advertisements did not arrive")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leases) < 3 || !leases[1].After(leases[0]) || !leases[2].After(leases[1]) {
		t.Fatalf("ownership lease did not advance: %v", leases)
	}
}
