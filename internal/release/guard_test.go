//go:build linux

package release

import (
	"context"
	"errors"
	"testing"
)

func TestWithStateKeepsProcessAndFileLocksUntilCallbackFinishes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	initializeFixture(t, s)
	next := stageFixture(t, s, "fixture-r2", 2)
	other, err := Open(s.options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	before := mustState(t, s)
	want := errors.New("callback failed")
	err = s.WithState(ctx, func(state State) error {
		if state.Generation != before.Generation {
			t.Fatal("wrong state")
		}
		if _, err := s.Status(ctx); !errors.Is(err, ErrBusy) {
			t.Fatalf("process lock: %v", err)
		}
		if _, err := other.Prepare(ctx, next, state.Generation, PrepareOptions{}); !errors.Is(err, ErrBusy) {
			t.Fatalf("file lock: %v", err)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
	if _, err := other.Prepare(ctx, next, before.Generation, PrepareOptions{}); err != nil {
		t.Fatalf("callback leaked lock: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.WithState(ctx, func(State) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}
