//go:build linux

package vrrp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

type transport interface {
	Receive(context.Context) ([]byte, error)
	Send(context.Context, []byte) error
	VerifyActive(context.Context, Snapshot) error
	Close() error
}

// Run owns only protocol sockets. Effects must synchronously reconcile kernel
// ownership and must honor its context deadline. On activation, the native
// transport verifies VMAC and non-tentative VIP presence after Effects returns;
// no positive-priority advertisement is sent before that check succeeds.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) error {
	cfg = Normalize(cfg)
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("VRRP requires an acknowledged ownership effects implementation")
	}
	device, source, err := openTransport(cfg)
	if err != nil {
		return err
	}
	defer device.Close()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	return run(ctx, cfg, source, hex.EncodeToString(id[:]), device, effects, ready)
}

func applyActions(ctx context.Context, m *machine, a actions, device transport, effects Effects) error {
	if a.update || a.advertise {
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		if err := m.stampObservation(clock); err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, time.Duration(m.config.EffectTimeoutMilliseconds)*time.Millisecond)
		if m.state == StateActive {
			m.validUntil = time.Now().Add(time.Duration(m.expiresNanos - uint64(clock.BoottimeNanoseconds)))
			leased, cancelLease := context.WithDeadline(operation, m.validUntil)
			defer cancelLease()
			operation = leased
		}
		err = effects.Update(operation, m.snapshot())
		cancel()
		if err != nil {
			return fmt.Errorf("VRRP ownership %s: %w", m.state, err)
		}
		if m.state == StateActive {
			clock, err := boottime.Read()
			if err != nil || !CurrentOwnership(m.snapshot(), clock) {
				return errors.Join(errors.New("VRRP ownership lease expired before acknowledgement"), err)
			}
		}
	}
	if a.advertise || a.notify {
		operation, cancel := context.WithTimeout(ctx, time.Duration(m.config.EffectTimeoutMilliseconds)*time.Millisecond)
		err := device.VerifyActive(operation, m.snapshot())
		cancel()
		if err != nil {
			return fmt.Errorf("VRRP active ownership verification: %w", err)
		}
	}
	if a.advertise {
		if err := sendAdvertisement(ctx, m, device, uint8(m.config.Priority)); err != nil {
			return err
		}
	}
	if a.notify {
		for _, frame := range notificationFrames(m.config) {
			clock, err := boottime.Read()
			if err != nil || !CurrentOwnership(m.snapshot(), clock) {
				return errors.Join(errors.New("VRRP ownership expired before neighbor announcement"), err)
			}
			operation, cancel := context.WithDeadline(ctx, minTime(time.Now().Add(250*time.Millisecond), m.validUntil))
			err = device.Send(operation, frame)
			cancel()
			if err != nil {
				return fmt.Errorf("VRRP neighbor announcement: %w", err)
			}
		}
		m.deadline = time.Now().Add(m.config.interval())
	}
	return nil
}

func sendAdvertisement(ctx context.Context, m *machine, device transport, priority uint8) error {
	if priority != 0 {
		clock, err := boottime.Read()
		if err != nil || !CurrentOwnership(m.snapshot(), clock) {
			return errors.Join(errors.New("VRRP ownership expired before advertisement"), err)
		}
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	if priority != 0 {
		deadline = minTime(deadline, m.validUntil)
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := operation.Err(); err != nil {
		return err
	}
	if err := device.Send(operation, advertisementFrame(m.config, m.source, priority)); err != nil {
		return fmt.Errorf("send VRRP advertisement: %w", err)
	}
	m.advertisements++
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func withdraw(m *machine, device transport, effects Effects) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(m.config.EffectTimeoutMilliseconds)*time.Millisecond+250*time.Millisecond)
	defer cancel()
	var sendErr error
	if m.state == StateActive && m.advertisements != 0 {
		// Priority zero accelerates the peer's failover while the departing
		// owner can still forward. Withdrawal then prevents further ownership.
		sendErr = sendAdvertisement(ctx, m, device, 0)
	}
	m.enter(StateInitialize, time.Now())
	clock, clockErr := boottime.Read()
	if clockErr == nil {
		clockErr = m.stampObservation(clock)
	}
	if clockErr != nil {
		// Revoke even if the clock becomes unavailable. Keep the last observed
		// boot/time; a zero deadline never creates authority or a new lease.
		m.expiresNanos = 0
	}
	applyErr := effects.Update(ctx, m.snapshot())
	return errors.Join(sendErr, clockErr, applyErr)
}

func run(ctx context.Context, cfg Config, source netip.Addr, epoch string, device transport, effects Effects, ready func()) (result error) {
	m := newMachine(cfg, source, epoch)
	defer func() {
		// Shutdown can interrupt an ownership action or neighbor announcement,
		// as well as the select below. Report normal parent cancellation as a
		// clean stop, then independently preserve any withdrawal failure.
		if cause := ctx.Err(); cause != nil {
			// Only a single wrapping chain ending at parent cancellation is
			// normal shutdown. A joined error can contain an independent failure.
			for err := result; err != nil; err = errors.Unwrap(err) {
				if err == cause {
					result = nil
					break
				}
			}
		}
		result = errors.Join(result, withdraw(m, device, effects))
	}()
	rxContext, cancelReceive := context.WithCancel(ctx)
	frames := make(chan []byte, 64)
	rxError, rxDone := make(chan error, 1), make(chan struct{})
	var dropped atomic.Uint64
	go func() {
		defer close(rxDone)
		for rxContext.Err() == nil {
			frame, err := device.Receive(rxContext)
			if err != nil {
				select {
				case rxError <- err:
				case <-rxContext.Done():
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
	defer func() {
		cancelReceive()
		<-rxDone // native receive uses nonblocking I/O and a bounded poll.
	}()
	if err := applyActions(ctx, m, m.start(time.Now()), device, effects); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	nextDiagnostic := time.Time{}
	observationTicker := time.NewTicker(time.Second)
	defer observationTicker.Stop()
	for {
		wait := time.Until(m.deadline)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		var action actions
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case err := <-rxError:
			timer.Stop()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive VRRP: %w", err)
		case frame := <-frames:
			timer.Stop()
			p, err := parseFrame(cfg, frame)
			if err != nil {
				if errors.Is(err, errOtherVirtualRouter) {
					continue
				}
				m.rejected++
				m.lastError = frameError(err)
				if time.Now().After(nextDiagnostic) {
					slog.Warn("discarded VRRP advertisement", "resource", cfg.Resource, "reason", err.Error(), "rejected", m.rejected)
					nextDiagnostic = time.Now().Add(time.Second)
				}
				continue
			}
			previousMismatch := m.intervalMismatch
			action = m.receive(time.Now(), p)
			if m.intervalMismatch != previousMismatch && time.Now().After(nextDiagnostic) {
				slog.Warn("VRRP advertisement interval mismatch", "resource", cfg.Resource, "peer", p.source.String(), "peer_interval", p.interval, "local_interval", cfg.interval())
				nextDiagnostic = time.Now().Add(time.Second)
			}
		case <-timer.C:
			action = m.tick(time.Now())
		case <-observationTicker.C:
			timer.Stop()
			// Backup is a current negative observation too. Do not leave its
			// readiness to expire merely because a peer keeps resetting the
			// election deadline without a local state transition.
			action.update = m.state == StateBackup
		}
		m.receiveDropped = dropped.Load()
		if err := applyActions(ctx, m, action, device, effects); err != nil {
			return err
		}
	}
}
