//go:build linux

package release

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestCheckedPrepareValidatesPinnedTargetUnderFileLockBeforePublication(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	target := stageFixture(t, s, "fixture-r2", 2)
	other, err := Open(s.options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	before := mustState(t, s)
	want := errors.New("live configuration revision changed")
	var borrowed *os.File
	checks := 0
	check := func(ctx context.Context, state State, image *Image) error {
		checks++
		if !reflect.DeepEqual(state, before) || image.Release != target {
			t.Fatal("preflight did not receive actual current state and verified target")
		}
		borrowed = image.Entrypoint()
		if _, err := borrowed.Stat(); err != nil {
			t.Fatal("candidate descriptor was closed", err)
		}
		if _, err := other.Status(ctx); !errors.Is(err, ErrBusy) {
			t.Fatalf("file lock was not held during preflight: %v", err)
		}
		// Modifying callback metadata must never mutate the commit's own state.
		state.Active.Digest = "modified callback view"
		return want
	}
	if _, err := s.PrepareChecked(ctx, target, before.Generation, PrepareOptions{}, check); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if checks != 1 || !reflect.DeepEqual(mustState(t, s), before) {
		t.Fatal("failed preflight consumed authority or published a candidate")
	}
	if _, err := borrowed.Stat(); err == nil {
		t.Fatal("failed preflight leaked borrowed target descriptor")
	}
	if _, err := s.PrepareChecked(ctx, target, before.Generation, PrepareOptions{}, nil); err == nil {
		t.Fatal("checked API accepted missing preflight")
	}
	invalid := pairOptions()
	invalid.Startup.Generation = 0
	called := false
	if _, err := s.PrepareChecked(ctx, target, before.Generation, invalid, func(context.Context, State, *Image) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatal("malformed pair invoked target preflight", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	if _, err := s.PrepareChecked(canceled, target, before.Generation, PrepareOptions{}, func(context.Context, State, *Image) error { cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled validation published candidate", err)
	}
	if !reflect.DeepEqual(mustState(t, s), before) {
		t.Fatal("canceled validation reserved generations")
	}
	state, err := s.PrepareChecked(ctx, target, before.Generation, PrepareOptions{}, func(_ context.Context, current State, image *Image) error {
		if current.Active == nil || *current.Active != base || image.Release != target {
			t.Fatal("wrong target or active state")
		}
		current.Active.Digest = "changed copy"
		return nil
	})
	if err != nil || state.Active == nil || *state.Active != base || state.Pending == nil || state.Pending.Release != target || state.Generation != before.Generation+1 {
		t.Fatal("successful preflight did not publish exact pair", state, err)
	}
	checks = 0
	if _, err := s.PrepareChecked(ctx, target, before.Generation, PrepareOptions{}, func(context.Context, State, *Image) error { checks++; return nil }); !errors.Is(err, ErrStale) || checks != 0 {
		t.Fatal("stale generation entered preflight", err)
	}
}

func TestCheckedRollbackUsesVerifiedPreviousRuntimeAndFrozenPairIntent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	next := stageFixture(t, s, "fixture-r2", 2)
	if _, err := s.Prepare(ctx, next, 1, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	boot, err := s.BeginBoot(ctx, "preflight-boot")
	if err != nil {
		t.Fatal(err)
	}
	boot.Image.Close()
	state, err := s.Confirm(ctx, "preflight-boot", boot.State.Attempt.Token, next.Digest)
	if err != nil {
		t.Fatal(err)
	}
	opts := pairOptions()
	want := opts.Startup.Clone()
	baseline := *opts.KnownGood
	if _, err := s.PrepareRollbackChecked(ctx, state.Generation, opts, nil); err == nil {
		t.Fatal("missing rollback preflight accepted")
	}
	got, err := s.PrepareRollbackChecked(ctx, state.Generation, opts, func(_ context.Context, current State, target *Image) error {
		if current.Active == nil || *current.Active != next || current.Previous == nil || *current.Previous != base || target.Release != base {
			t.Fatal("rollback validated wrong runtime")
		}
		current.Previous.Digest = "changed view"
		opts.Startup.Generation = 100
		opts.KnownGood.Digest = "changed caller reference"
		return nil
	})
	if err != nil || got.Pending == nil || got.Pending.Release != base || !got.Pending.Startup.Equal(want) || got.Pending.Fallback.Snapshot != baseline || got.HighWatermark != next.Sequence {
		t.Fatal("callback changed selected pair or reserved watermarks", got, err)
	}
}
