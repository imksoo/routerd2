//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Run owns one single-hop interface/family until cancellation or a fatal socket,
// interface or publisher failure. No routing configuration is changed. Initial
// Down status is acknowledged before ready; the last active Update is joined
// before final inactive status. Effects must honor their context deadlines.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) error {
	cfg = Normalize(cfg)
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("BFD status effects are required")
	}
	if ctx.Err() != nil {
		return nil
	}
	clock, err := ReadClock()
	if err != nil {
		return err
	}
	device, err := openTransport(cfg, clock)
	if err != nil {
		return fmt.Errorf("open BFD transport: %w", err)
	}
	return run(ctx, cfg, device, effects, ready, func() (Clock, error) { return clockForBoot(clock.BootID) }, rand.Uint32N)
}

type worker struct {
	config           Config
	sessions         []*session
	byDisc           map[uint32]int
	bySource         map[netip.Addr]int
	source           netip.Addr
	clock            Clock
	rejected         uint64
	lastError        string
	lastTimestamp    time.Time
	timestampChanges uint64
}

func newWorker(cfg Config, device transport, clock Clock, jitter func(uint32) uint32) (*worker, error) {
	ports, discriminators := device.Ports(), device.Discriminators()
	if len(ports) != len(cfg.Peers) || len(discriminators) != len(cfg.Peers) || !validBootID(clock.BootID) || clock.BoottimeNanoseconds <= 0 || !validWall(clock.WallTime) || jitter == nil {
		return nil, errors.New("invalid BFD transport identity or clock")
	}
	w := &worker{config: cfg, source: netip.MustParseAddr(cfg.SourceAddress), clock: clock, byDisc: map[uint32]int{}, bySource: map[netip.Addr]int{}}
	for i, peer := range cfg.Peers {
		if _, exists := w.byDisc[discriminators[i]]; exists || discriminators[i] == 0 || ports[i] < 49152 {
			return nil, errors.New("BFD transport did not reserve unique nonzero identities")
		}
		w.sessions = append(w.sessions, newSession(peer, discriminators[i], ports[i], clock.BoottimeNanoseconds, jitter))
		w.byDisc[discriminators[i]], w.bySource[netip.MustParseAddr(peer.Address)] = i, i
	}
	return w, nil
}

func (w *worker) snapshot(active bool, dropped uint64) Snapshot {
	// Count produced timestamp changes before the publisher can coalesce them.
	// Equality can distinguish A->B->A; notEmpty need not reset on each renewal.
	if !w.clock.WallTime.Equal(w.lastTimestamp) {
		increment(&w.timestampChanges)
		w.lastTimestamp = w.clock.WallTime
	}
	s := Snapshot{Resource: w.config.Resource, Interface: w.config.Interface, SourceAddress: w.config.SourceAddress, BootID: w.clock.BootID, Active: active, ObservedNanos: uint64(w.clock.BoottimeNanoseconds), ExpiresNanos: uint64(w.clock.BoottimeNanoseconds) + uint64(StatusLifetime), TimestampChanges: w.timestampChanges, UpdatedAt: w.clock.WallTime, Rejected: w.rejected, ReceiveDropped: dropped, LastError: w.lastError, Peers: make([]PeerState, len(w.sessions))}
	for i, peer := range w.sessions {
		s.Peers[i] = peer.snapshot(active)
	}
	return s
}

func shortError(err error) string {
	message := err.Error()
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}

func (w *worker) reject(peer int, err error) {
	increment(&w.rejected)
	if peer >= 0 {
		increment(&w.sessions[peer].rejected)
	}
	w.lastError = shortError(err)
}

func (w *worker) accept(d datagram) (int, bool, bool) {
	if d.invalid || d.hopLimit != 255 || d.index <= 0 || d.destination != w.source || d.sourcePort < 49152 || d.source.Is4() != w.source.Is4() || d.source == w.source || d.received.BootID != w.clock.BootID || !validWall(d.received.WallTime) {
		w.reject(-1, errors.New("BFD datagram has invalid interface, address, hop limit, port or receive clock"))
		return -1, false, false
	}
	if _, ok := unicast(d.source.String()); !ok {
		w.reject(-1, errors.New("BFD datagram source is not unicast"))
		return -1, false, false
	}
	p, err := parseControl(d.data)
	if err != nil {
		w.reject(-1, err)
		return -1, false, false
	}
	var i int
	var found bool
	if p.your != 0 {
		i, found = w.byDisc[p.your]
	} else if w.config.PointToPoint {
		i, found = 0, true
	} else {
		i, found = w.bySource[d.source]
	}
	if !found || !w.config.PointToPoint && w.sessions[i].config.Address != d.source.String() {
		w.reject(-1, errors.New("BFD discriminator and source do not identify a configured peer"))
		return -1, false, false
	}
	changed, final, err := w.sessions[i].receive(p, d.source.String(), d.received, w.clock.BoottimeNanoseconds)
	if err != nil {
		w.reject(i, err)
		return -1, false, false
	}
	return i, changed, final
}

func (w *worker) send(device transport, i int, final bool) error {
	err := device.Send(i, w.sessions[i].packet(final).bytes())
	w.sessions[i].sent(w.clock.BoottimeNanoseconds, final, err == nil)
	if err != nil {
		w.lastError = shortError(err)
		if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTSOCK) || errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("BFD transmit socket failed: %w", err)
		}
	}
	return nil
}

func (w *worker) publishInterval() int64 {
	interval := int64(200 * time.Millisecond)
	for _, s := range w.sessions {
		if s.state == wireInit || s.state == wireUp {
			interval = min(interval, int64(s.detectionTime())*1000/3)
		}
	}
	return max(interval, int64(5*time.Millisecond))
}

func update(ctx context.Context, effects Effects, timeout time.Duration, s Snapshot) error {
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := effects.Update(call, s)
	return errors.Join(err, call.Err())
}

// A single-slot mailbox bounds memory and coalesces intermediate refreshes.
// There is one writer and one reader. The reader serializes all active calls.
func publishLatest(queue chan Snapshot, s Snapshot) {
	select {
	case queue <- s:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	queue <- s
}

func run(ctx context.Context, cfg Config, device transport, effects Effects, ready func(), readClock func() (Clock, error), jitter func(uint32) uint32) (result error) {
	defer func() { result = errors.Join(result, device.Close()) }()
	clock, err := readClock()
	if err != nil {
		return err
	}
	w, err := newWorker(cfg, device, clock, jitter)
	if err != nil {
		return err
	}
	timeout := time.Duration(cfg.EffectTimeoutMilliseconds) * time.Millisecond
	var dropped atomic.Uint64
	// Installed before any active publication so a partially applied initial
	// Update also receives a fenced withdrawal. Abstract ownership is released
	// only after this last Update has returned.
	defer func() {
		if current, err := readClock(); err == nil && current.BootID == w.clock.BootID && current.BoottimeNanoseconds >= w.clock.BoottimeNanoseconds {
			w.clock = current
		}
		for i, s := range w.sessions {
			s.enter(wireAdminDown, 7, w.clock.BoottimeNanoseconds)
			// Best effort notification is independent of periodic suppression.
			_ = w.send(device, i, false)
		}
		result = errors.Join(result, update(context.Background(), effects, timeout, w.snapshot(false, dropped.Load())))
	}()
	if err := update(ctx, effects, timeout, w.snapshot(true, 0)); err != nil {
		return fmt.Errorf("publish initial BFD status: %w", err)
	}
	if ctx.Err() != nil {
		return nil
	}
	ioContext, cancelIO := context.WithCancel(ctx)
	rxDone, inspectDone, publishDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	failures := make(chan error, 3)
	frames, pending := make(chan datagram, 256), make(chan Snapshot, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		case <-ioContext.Done():
		}
	}
	go func() {
		defer close(rxDone)
		for ioContext.Err() == nil {
			frame, err := device.Receive(ioContext)
			if err != nil {
				if ioContext.Err() == nil {
					fail(fmt.Errorf("receive BFD: %w", err))
				}
				return
			}
			select {
			case frames <- frame:
			default:
				dropped.Add(1)
			}
		}
	}()
	go func() {
		defer close(inspectDone)
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ioContext.Done():
				return
			case <-timer.C:
				if err := device.Verify(); err != nil {
					fail(fmt.Errorf("verify BFD interface: %w", err))
					return
				}
			}
		}
	}()
	go func() {
		defer close(publishDone)
		for {
			select {
			case <-ioContext.Done():
				return
			case s := <-pending:
				if ioContext.Err() != nil {
					return
				}
				if err := update(ioContext, effects, timeout, s); err != nil {
					if ioContext.Err() == nil {
						fail(fmt.Errorf("publish BFD status: %w", err))
					}
					return
				}
			}
		}
	}()
	defer func() {
		cancelIO()
		<-rxDone
		<-inspectDone
		<-publishDone // no old active Update can follow the inactive fence.
	}()
	if ready != nil {
		ready()
	}
	nextPublish, dirty := clock.BoottimeNanoseconds+w.publishInterval(), false
	var received *datagram
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case err := <-failures:
			return err
		default:
		}
		clock, err = readClock()
		if err != nil {
			return err
		}
		if clock.BootID != w.clock.BootID || clock.BoottimeNanoseconds < w.clock.BoottimeNanoseconds || !validWall(clock.WallTime) {
			return errors.New("BFD monotonic clock identity changed")
		}
		w.clock = clock
		now := clock.BoottimeNanoseconds
		// Expiry precedes queued input after a worker stall; an old Up cannot
		// survive suspension merely because valid packets were queued meanwhile.
		for _, s := range w.sessions {
			dirty = s.expire(now) || dirty
		}
		if received != nil {
			i, changed, final := w.accept(*received)
			dirty = dirty || changed
			if i >= 0 && final {
				if err := w.send(device, i, true); err != nil {
					return err
				}
			}
			received = nil
		}
		for i, s := range w.sessions {
			if s.nextTx != 0 && now >= s.nextTx {
				if err := w.send(device, i, false); err != nil {
					return err
				}
			}
		}
		if dirty || now >= nextPublish {
			publishLatest(pending, w.snapshot(true, dropped.Load()))
			nextPublish, dirty = now+w.publishInterval(), false
		}
		next := nextPublish
		for _, s := range w.sessions {
			if s.nextTx != 0 {
				next = min(next, s.nextTx)
			}
			if s.remoteDeadline != 0 {
				next = min(next, s.remoteDeadline)
			}
		}
		timer := time.NewTimer(time.Duration(max(next-now, 0)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case err := <-failures:
			timer.Stop()
			return err
		case frame := <-frames:
			timer.Stop()
			received = &frame
		case <-timer.C:
		}
	}
}
