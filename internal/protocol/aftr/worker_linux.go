//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func currentInstant() (instant, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return instant{}, err
	}
	return instant{wall: time.Now().UTC(), boot: time.Unix(ts.Sec, ts.Nsec).UTC()}, nil
}

type lookupFunc func(context.Context, Config, Input) (resolution, error)
type lookupResult struct {
	generation, sequence uint64
	value                resolution
	err                  error
}

// Run consumes complete compiler inputs, not kernel state. ready means the
// initial bounded snapshot was acknowledged; it does not imply DNS or tunnel
// availability. Errors from the update stream, validation or callback are
// fatal. DNS failures are explicit status and preserve last-good data only
// until all of its original source/answer lifetimes remain usable.
func Run(ctx context.Context, c Config, initial Input, updates <-chan Input, effects Effects, ready func()) error {
	return run(ctx, c, initial, updates, effects, ready, resolve)
}

func run(ctx context.Context, c Config, initial Input, updates <-chan Input, effects Effects, ready func(), lookup lookupFunc) (result error) {
	if err := ValidateInput(c, initial); err != nil {
		return err
	}
	if effects == nil || lookup == nil {
		return errors.New("AFTR effects and lookup are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now, err := currentInstant()
	if err != nil {
		return err
	}
	m := &machine{config: c}
	if _, err := m.apply(initial, now); err != nil {
		return err
	}
	var lastView Snapshot
	published := false
	publish := func(now instant, force bool) error {
		view := m.view(now)
		view.UpdatedAt = time.Time{}
		shift := clockShift(m.clock, now)
		if !force && published && reflect.DeepEqual(view, lastView) && shift < time.Second && shift > -time.Second {
			return nil
		}
		s := m.snapshot(now)
		if err := ValidateSnapshot(c, m.input, s); err != nil {
			return fmt.Errorf("invalid AFTR produced snapshot: %w", err)
		}
		if err := effects.Update(ctx, s); err != nil {
			return fmt.Errorf("publish AFTR snapshot: %w", err)
		}
		lastView, published = view, true
		return nil
	}
	if err := publish(now, true); err != nil {
		return err
	}
	defer func() {
		now, err := currentInstant()
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		s := Snapshot{Resource: c.Resource, Interface: c.Interface, ConfigDigest: c.ConfigDigest, Generation: m.input.Generation, CandidateIndex: -1, ResolverIndex: -1, UpdatedAt: now.wall, Attempts: m.attempts, Failures: m.failures, LastError: m.lastError}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := effects.Update(cleanup, s); err != nil {
			result = errors.Join(result, fmt.Errorf("withdraw AFTR snapshot: %w", err))
		}
	}()
	if ready != nil {
		ready()
	}
	results := make(chan lookupResult, 1)
	var cancelQuery context.CancelFunc
	var workers sync.WaitGroup
	var sequence uint64
	cancel := func() {
		if cancelQuery != nil {
			cancelQuery()
			cancelQuery = nil
		}
		sequence++
	}
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now, err = currentInstant()
		if err != nil {
			return err
		}
		if m.dependencyError(now.boot) != "" {
			if cancelQuery != nil {
				cancel()
			}
			m.nextAttempt = time.Time{}
		} else if cancelQuery == nil && m.nextAttempt.IsZero() && !m.view(now).Available {
			m.nextAttempt = now.boot
		}
		if m.dependencyError(now.boot) == "" && cancelQuery == nil && !m.nextAttempt.IsZero() && !now.boot.Before(m.nextAttempt) {
			queryCtx, queryCancel := context.WithCancel(ctx)
			cancelQuery = queryCancel
			sequence++
			seq, input := sequence, cloneInput(m.learned)
			m.attempts++
			m.nextAttempt = time.Time{}
			workers.Add(1)
			go func() {
				defer workers.Done()
				value, err := lookup(queryCtx, c, input)
				select {
				case results <- lookupResult{generation: input.Generation, sequence: seq, value: value, err: err}:
				case <-queryCtx.Done():
				}
			}()
		}
		if err := publish(now, false); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case in, ok := <-updates:
			if !ok {
				return errors.New("AFTR input stream closed")
			}
			now, err = currentInstant()
			if err != nil {
				return err
			}
			changed, err := m.apply(in, now)
			if err != nil {
				return err
			}
			if changed {
				cancel()
			}
		case r := <-results:
			if r.generation != m.input.Generation || r.sequence != sequence {
				continue
			}
			cancel()
			now, err = currentInstant()
			if err != nil {
				return err
			}
			m.result(r.value, r.err, now)
		}
	}
}
