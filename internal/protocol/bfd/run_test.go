//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type sentPacket struct {
	peer int
	data []byte
}
type fakeTransport struct {
	frames    chan datagram
	sent      chan sentPacket
	fail      chan error
	closed    atomic.Int32
	transmits atomic.Int32
	sendError error
	peers     int
}

func fakeDevice(peers int) *fakeTransport {
	return &fakeTransport{frames: make(chan datagram, 512), sent: make(chan sentPacket, 4096), fail: make(chan error, 1), peers: peers}
}
func (d *fakeTransport) Receive(ctx context.Context) (datagram, error) {
	select {
	case <-ctx.Done():
		return datagram{}, ctx.Err()
	case err := <-d.fail:
		return datagram{}, err
	case frame := <-d.frames:
		return frame, nil
	}
}
func (d *fakeTransport) Send(peer int, data []byte) error {
	d.transmits.Add(1)
	select {
	case d.sent <- sentPacket{peer: peer, data: append([]byte(nil), data...)}:
	default:
	}
	return d.sendError
}
func (d *fakeTransport) Verify() error { return nil }
func (d *fakeTransport) Ports() []uint16 {
	ports := make([]uint16, d.peers)
	for i := range ports {
		ports[i] = 49160 + uint16(i)
	}
	return ports
}
func (d *fakeTransport) Discriminators() []uint32 {
	ids := make([]uint32, d.peers)
	for i := range ids {
		ids[i] = 17 + uint32(i)
	}
	return ids
}
func (d *fakeTransport) Close() error { d.closed.Add(1); return nil }

func runningClock() func() (Clock, error) {
	start := time.Now()
	return func() (Clock, error) { return sampleClock(int64(time.Second) + int64(time.Since(start))), nil }
}

func frameFrom(cfg Config, p control, clock Clock) datagram {
	return datagram{data: p.bytes(), source: netip.MustParseAddr(cfg.Peers[0].Address), destination: netip.MustParseAddr(cfg.SourceAddress), sourcePort: 49170, index: 3, hopLimit: 255, received: clock}
}

func waitUntil(t *testing.T, check func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("BFD worker did not finish bounded cancellation")
		return nil
	}
}

func TestInitialAckReadyAndFinalFence(t *testing.T) {
	cfg, device := testConfig(), fakeDevice(1)
	clock := runningClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var calls []Snapshot
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if err := ValidateSnapshot(cfg, s); err != nil {
			return err
		}
		mu.Lock()
		calls = append(calls, s)
		first := len(calls) == 1
		mu.Unlock()
		if first {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, device, effects, func() { close(ready) }, clock, func(uint32) uint32 { return 0 })
	}()
	<-started
	select {
	case <-ready:
		t.Fatal("ready was called before initial publication acknowledgment")
	default:
	}
	if device.transmits.Load() != 0 {
		t.Fatal("protocol started before initial acknowledgment")
	}
	close(release)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("ready did not follow initial ack")
	}
	waitUntil(t, func() bool { return device.transmits.Load() > 0 }, "active initial Down was not sent")
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 || !calls[0].Active || calls[0].Peers[0].State != StateDown || calls[len(calls)-1].Active || calls[len(calls)-1].Peers[0].State != StateAdminDown || device.closed.Load() != 1 {
		t.Fatalf("initial/withdrawal ownership fence invalid: %+v", calls)
	}
}

func TestFailedInitialPublicationNeverReadyAndStillWithdraws(t *testing.T) {
	cfg, device := testConfig(), fakeDevice(1)
	marker := errors.New("initial publisher failed")
	ready, withdrawn := false, false
	err := run(context.Background(), cfg, device, EffectsFunc(func(_ context.Context, s Snapshot) error {
		if s.Active {
			return marker
		}
		withdrawn = true
		return ValidateSnapshot(cfg, s)
	}), func() { ready = true }, runningClock(), func(uint32) uint32 { return 0 })
	if !errors.Is(err, marker) || ready || !withdrawn || device.closed.Load() != 1 {
		t.Fatalf("failed ack leaked lifecycle: ready=%t withdrawn=%t err=%v", ready, withdrawn, err)
	}
}

func TestSlowEffectsDoNotBlockWireAndCoalescingRetainsDownTransitions(t *testing.T) {
	cfg, device, clock := testConfig(), fakeDevice(1), runningClock()
	cfg.EffectTimeoutMilliseconds = 2000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked, release := make(chan struct{}), make(chan struct{})
	var blockedOnce atomic.Bool
	var mu sync.Mutex
	var last Snapshot
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if err := ValidateSnapshot(cfg, s); err != nil {
			return err
		}
		if s.Active && s.Peers[0].State == StateUp && blockedOnce.CompareAndSwap(false, true) {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		mu.Lock()
		last = s
		mu.Unlock()
		return nil
	})
	done, ready := make(chan error, 1), make(chan struct{})
	go func() {
		done <- run(ctx, cfg, device, effects, func() { close(ready) }, clock, func(uint32) uint32 { return 0 })
	}()
	<-ready
	send := func(state uint8) { now, _ := clock(); device.frames <- frameFrom(cfg, peerPacket(state, 17), now) }
	send(wireInit)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("Up publication was not reached")
	}
	before := device.transmits.Load()
	stopPeer := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPeer:
				return
			case <-ticker.C:
				send(wireUp)
			}
		}
	}()
	waitUntil(t, func() bool { return device.transmits.Load() >= before+3 }, "slow Effects stopped 50ms packet timers")
	close(stopPeer)
	<-peerDone
	send(wireDown)
	send(wireInit)
	// Feed Up until after the blocked in-flight Update has returned. The pending
	// latest Up must retain the loss that occurred between two acknowledgments.
	for i := 0; i < 5; i++ {
		send(wireUp)
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return last.Active && len(last.Peers) == 1 && last.Peers[0].State == StateUp && last.Peers[0].DownTransitions == 1 && last.Peers[0].UpTransitions == 2
	}, "coalescing erased a short loss/recovery")
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if last.Active || last.Peers[0].State != StateAdminDown || last.Peers[0].DeadlineBootNanoseconds != 0 {
		t.Fatal("final inactive status was not last")
	}
}

func TestCancellationJoinsBlockedPublisherBeforeInactive(t *testing.T) {
	cfg, device, clock := testConfig(), fakeDevice(1), runningClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	var calls atomic.Int32
	var mu sync.Mutex
	var order []string
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if calls.Add(1) == 2 {
			close(blocked)
			<-ctx.Done()
			mu.Lock()
			order = append(order, "active-return")
			mu.Unlock()
			return ctx.Err()
		}
		if !s.Active {
			mu.Lock()
			order = append(order, "inactive")
			mu.Unlock()
		}
		return nil
	})
	done, ready := make(chan error, 1), make(chan struct{})
	go func() {
		done <- run(ctx, cfg, device, effects, func() { close(ready) }, clock, func(uint32) uint32 { return 0 })
	}()
	<-ready
	now, _ := clock()
	device.frames <- frameFrom(cfg, peerPacket(wireInit, 17), now)
	<-blocked
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "active-return" || order[1] != "inactive" {
		t.Fatalf("stale active publication can cross withdrawal: %v", order)
	}
}

func TestRuntimeFailureWithdrawsAndTransientSendDoesNotSpin(t *testing.T) {
	for _, mode := range []string{"receive", "publish", "fatal-send", "transient-send"} {
		t.Run(mode, func(t *testing.T) {
			cfg, device, clock := testConfig(), fakeDevice(1), runningClock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			marker := errors.New("injected BFD worker failure")
			if mode == "fatal-send" {
				device.sendError = unix.EBADF
			}
			if mode == "transient-send" {
				device.sendError = unix.ENETDOWN
			}
			var withdrawn atomic.Bool
			var calls atomic.Int32
			effects := EffectsFunc(func(_ context.Context, s Snapshot) error {
				if !s.Active {
					withdrawn.Store(true)
					return nil
				}
				if calls.Add(1) >= 2 && mode == "publish" {
					return marker
				}
				return nil
			})
			done, ready := make(chan error, 1), make(chan struct{})
			go func() {
				done <- run(ctx, cfg, device, effects, func() { close(ready) }, clock, func(uint32) uint32 { return 0 })
			}()
			<-ready
			if mode == "receive" {
				device.fail <- marker
			}
			if mode == "publish" {
				now, _ := clock()
				device.frames <- frameFrom(cfg, peerPacket(wireInit, 17), now)
			}
			if mode == "transient-send" {
				time.Sleep(100 * time.Millisecond)
				if count := device.transmits.Load(); count > 2 || count < 1 {
					t.Fatalf("failed sends busy-looped: %d", count)
				}
				cancel()
			}
			err := waitDone(t, done)
			if mode != "transient-send" && err == nil {
				t.Fatal("fatal failure did not stop worker")
			}
			if mode == "transient-send" && err != nil {
				t.Fatal(err)
			}
			if !withdrawn.Load() || device.closed.Load() != 1 {
				t.Fatal("failed worker retained ownership")
			}
		})
	}
}

func TestDemultiplexingAndRejectedPacketsNeverRefresh(t *testing.T) {
	cfg := testConfig()
	cfg.Peers = append(cfg.Peers, Peer{Address: "192.0.2.3", MinTxMilliseconds: 50, MinRxMilliseconds: 50, DetectMultiplier: 3})
	w, err := newWorker(cfg, fakeDevice(2), sampleClock(int64(time.Second)), func(uint32) uint32 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	good := frameFrom(cfg, peerPacket(wireInit, 17), w.clock)
	if i, changed, _ := w.accept(good); i != 0 || !changed {
		t.Fatal("configured peer did not establish Up")
	}
	deadline := w.sessions[0].deadline
	tests := map[string]func(*datagram){
		"wrong discriminator":   func(d *datagram) { p := peerPacket(wireUp, 18); d.data = p.bytes() },
		"unknown discriminator": func(d *datagram) { p := peerPacket(wireUp, 99); d.data = p.bytes() },
		"foreign source":        func(d *datagram) { d.source = netip.MustParseAddr("192.0.2.99") },
		"ttl":                   func(d *datagram) { d.hopLimit = 254 },
		"source port":           func(d *datagram) { d.sourcePort = 3784 },
		"destination":           func(d *datagram) { d.destination = netip.MustParseAddr("192.0.2.10") },
		"different boot":        func(d *datagram) { d.received.BootID = "ffffffff-ffff-ffff-ffff-ffffffffffff" },
		"future receive":        func(d *datagram) { d.received.BoottimeNanoseconds++ },
		"old receive":           func(d *datagram) { d.received.BoottimeNanoseconds-- },
		"invalid ancillary":     func(d *datagram) { d.invalid = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d := good
			mutate(&d)
			if i, _, _ := w.accept(d); i >= 0 {
				t.Fatal("foreign/invalid packet accepted")
			}
			if w.sessions[0].received != 1 || w.sessions[0].deadline != deadline || w.sessions[1].received != 0 {
				t.Fatal("rejected packet changed health or another peer")
			}
		})
	}
	if w.rejected != uint64(len(tests)) {
		t.Fatal("rejected packets were not counted")
	}
}

func TestPointToPointRemoteSourceMayChangeWithoutRetargeting(t *testing.T) {
	cfg := testConfig()
	cfg.PointToPoint = true
	w, err := newWorker(cfg, fakeDevice(1), sampleClock(int64(time.Second)), func(uint32) uint32 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	d := frameFrom(cfg, peerPacket(wireDown, 0), w.clock)
	d.source = netip.MustParseAddr("198.51.100.9")
	if i, _, _ := w.accept(d); i != 0 || w.sessions[0].state != wireInit {
		t.Fatal("initial point-to-point source alias rejected")
	}
	d.data, d.source = peerPacket(wireInit, 17).bytes(), netip.MustParseAddr("198.51.100.10")
	if i, _, _ := w.accept(d); i != 0 || w.sessions[0].state != wireUp {
		t.Fatal("point-to-point discriminator did not allow source change")
	}
	if w.sessions[0].config.Address != "192.0.2.2" || w.sessions[0].observedSource != "198.51.100.10" {
		t.Fatal("received source retargeted configured destination")
	}
	if err := ValidateSnapshot(cfg, w.snapshot(true, 0)); err != nil {
		t.Fatal(err)
	}
}
