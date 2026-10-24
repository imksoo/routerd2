// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func liveInput() Input {
	in := inputForTest()
	now := time.Now().UTC()
	in.UpdatedAt = now
	in.Source.PreferredUntil = now.Add(10 * time.Second)
	in.Source.ValidUntil = now.Add(20 * time.Second)
	in.Candidates[0].ValidUntil = now.Add(20 * time.Second)
	in.Resolvers[0].ValidUntil = now.Add(20 * time.Second)
	return in
}

func TestWorkerDiscardsOldQueryGenerationAndWithdraws(t *testing.T) {
	cfg, in := configForTest(), liveInput()
	updates := make(chan Input, 4)
	snapshots := make(chan Snapshot, 32)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseOld:
		default:
			close(releaseOld)
		}
	}()
	var calls atomic.Int32
	lookup := func(_ context.Context, _ Config, input Input) (resolution, error) {
		count := calls.Add(1)
		if count == 1 {
			close(oldStarted)
			<-releaseOld
		}
		now, _ := currentInstant()
		r := goodResult(now, 5)
		if input.Generation == 2 {
			r.answers[0].Address = "2001:db8:ffff::2"
		}
		return r, nil
	}
	go func() {
		done <- run(ctx, cfg, in, updates, EffectsFunc(func(_ context.Context, s Snapshot) error { snapshots <- s; return nil }), nil, lookup)
	}()
	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("initial query not started")
	}
	next := cloneInput(in)
	next.Generation = 2
	next.Candidates[0].Value = "new.example"
	updates <- next
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	found := false
	for !found {
		select {
		case s := <-snapshots:
			if s.Available {
				if s.Generation != 2 || s.Remote != "2001:db8:ffff::2" {
					t.Fatal("old query was applied")
				}
				found = true
			}
		case err := <-done:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatal("new-generation query not applied")
		}
	}
	close(releaseOld)
	// A delayed earlier lookup must not replace the current generation.
	for until := time.After(200 * time.Millisecond); ; {
		select {
		case s := <-snapshots:
			if s.Available && (s.Generation != 2 || s.Remote != "2001:db8:ffff::2") {
				t.Fatal("late result replaced current endpoint")
			}
		case <-until:
			goto stopped
		}
	}
stopped:
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel")
	}
	withdrawn := false
	for len(snapshots) > 0 {
		s := <-snapshots
		if !s.Active && !s.Available && s.Generation == 2 {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatal("shutdown did not withdraw current generation")
	}
}

func TestWorkerExpiresSourceWhileDNSLookupIsBlocked(t *testing.T) {
	cfg, in := configForTest(), liveInput()
	in.Source.PreferredUntil = time.Now().UTC().Add(250 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snapshots := make(chan Snapshot, 32)
	done := make(chan error, 1)
	queryStopped := make(chan struct{})
	lookup := func(ctx context.Context, _ Config, _ Input) (resolution, error) {
		<-ctx.Done()
		close(queryStopped)
		return resolution{}, ctx.Err()
	}
	go func() {
		done <- run(ctx, cfg, in, nil, EffectsFunc(func(_ context.Context, s Snapshot) error { snapshots <- s; return nil }), nil, lookup)
	}()
	select {
	case <-queryStopped:
	case <-time.After(time.Second):
		t.Fatal("expired source did not cancel in-flight DNS")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stuck behind DNS")
	}
	for len(snapshots) > 0 {
		if (<-snapshots).Available {
			t.Fatal("unresolved expired source produced an endpoint")
		}
	}
}

func TestWorkerRejectsChangedSameGenerationAndCallbackFailure(t *testing.T) {
	cfg, in := configForTest(), liveInput()
	updates := make(chan Input, 1)
	next := cloneInput(in)
	next.Source.Address = "2001:db8:1::2"
	updates <- next
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := run(ctx, cfg, in, updates, EffectsFunc(func(context.Context, Snapshot) error { return nil }), nil, func(ctx context.Context, _ Config, _ Input) (resolution, error) {
		<-ctx.Done()
		return resolution{}, ctx.Err()
	})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("bad generation was not rejected")
	}
	ready := false
	err = run(ctx, cfg, in, nil, EffectsFunc(func(context.Context, Snapshot) error { return errors.New("writer unavailable") }), func() { ready = true }, nilLookup)
	if err == nil || ready {
		t.Fatal("callback failure was swallowed or announced ready")
	}
}
func nilLookup(context.Context, Config, Input) (resolution, error) {
	return resolution{}, errors.New("unreachable test lookup")
}
