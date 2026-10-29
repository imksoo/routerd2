// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInitializePrepareBootConfirmRollbackAndWatermark(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	state := mustState(t, store)
	if state.Generation != 1 || *state.Active != first || state.HighWatermark != 1 {
		t.Fatalf("initial state: %+v", state)
	}
	if _, err := store.Initialize(ctx, second); err == nil {
		t.Fatal("second initialization accepted")
	}
	if _, err := store.Prepare(ctx, second, 0, PrepareOptions{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale prepare: %v", err)
	}
	prepared, err := store.Prepare(ctx, second, state.Generation, PrepareOptions{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if *prepared.Active != first || prepared.Previous != nil || prepared.Attempt != nil || prepared.Pending.Release != second || prepared.HighWatermark != 2 {
		t.Fatalf("prepare changed running selection: %+v", prepared)
	}
	if _, err := store.Prepare(ctx, second, prepared.Generation, PrepareOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("parallel candidate accepted: %v", err)
	}
	boot, err := store.BeginBoot(ctx, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Image.Close()
	if !boot.Trial || boot.RolledBack || boot.Image.Release != second || *boot.State.Previous != first || boot.State.Attempt.Number != 1 {
		t.Fatalf("trial: %+v", boot)
	}
	same, err := store.BeginBoot(ctx, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	same.Image.Close()
	if !reflect.DeepEqual(same.State, boot.State) {
		t.Fatal("duplicate boot ID consumed another attempt")
	}
	if _, err := store.CancelPending(ctx, boot.State.Generation); !errors.Is(err, ErrBusy) {
		t.Fatalf("trial falsely canceled as if untried: %v", err)
	}
	token := boot.State.Attempt.Token
	for _, arguments := range [][3]string{{"wrong-boot", token, second.Digest}, {"boot-1", "bad-token", second.Digest}, {"boot-1", token, first.Digest}} {
		if _, err := store.Confirm(ctx, arguments[0], arguments[1], arguments[2]); !errors.Is(err, ErrStale) {
			t.Fatalf("wrong confirmation accepted: %v", err)
		}
	}
	confirmed, err := store.Confirm(ctx, "boot-1", token, second.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Pending != nil || confirmed.Attempt != nil || *confirmed.Active != second || *confirmed.Previous != first {
		t.Fatalf("confirmation: %+v", confirmed)
	}
	if _, err := store.Confirm(ctx, "boot-1", token, second.Digest); !errors.Is(err, ErrStale) {
		t.Fatalf("confirmation replay: %v", err)
	}
	again, err := Open(store.options)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if restored := mustState(t, again); !reflect.DeepEqual(restored, confirmed) {
		t.Fatalf("restart lost state: %+v", restored)
	}
	rolled, err := again.Rollback(ctx, confirmed.Generation, "operator selected the previous known-good release")
	if err != nil {
		t.Fatal(err)
	}
	if *rolled.Active != first || *rolled.Previous != second || rolled.HighWatermark != 2 {
		t.Fatalf("explicit rollback: %+v", rolled)
	}
	if _, err := again.Prepare(ctx, second, rolled.Generation, PrepareOptions{}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("rollback reset high watermark: %v", err)
	}
	third := stageFixture(t, again, "fixture-r3", 3)
	prepared, err = again.Prepare(ctx, third, rolled.Generation, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := again.CancelPending(ctx, prepared.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if *canceled.Active != first || canceled.HighWatermark != 3 || canceled.Pending != nil {
		t.Fatalf("cancellation lost watermark: %+v", canceled)
	}
	if _, err := again.Prepare(ctx, third, canceled.Generation, PrepareOptions{}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("canceled release replay: %v", err)
	}
}

func TestUnconfirmedBootsRetryThenReturnKnownGoodPair(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	if _, err := store.Prepare(ctx, second, 1, PrepareOptions{MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	firstTrial, err := store.BeginBoot(ctx, "kernel-boot-1")
	if err != nil {
		t.Fatal(err)
	}
	firstTrial.Image.Close()
	secondTrial, err := store.BeginBoot(ctx, "kernel-boot-2")
	if err != nil {
		t.Fatal(err)
	}
	secondTrial.Image.Close()
	if secondTrial.State.Attempt.Number != 2 || secondTrial.State.Attempt.Token == firstTrial.State.Attempt.Token {
		t.Fatal("retry did not fence the earlier boot")
	}
	if _, err := store.Confirm(ctx, "kernel-boot-1", firstTrial.State.Attempt.Token, second.Digest); !errors.Is(err, ErrStale) {
		t.Fatalf("old boot confirmed new attempt: %v", err)
	}
	fallback, err := store.BeginBoot(ctx, "kernel-boot-3")
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Image.Close()
	if fallback.Trial || !fallback.RolledBack || fallback.Image.Release != first || fallback.State.Previous != nil || fallback.State.Pending != nil || fallback.State.Attempt != nil || fallback.State.LastFailure.Release != second || fallback.State.HighWatermark != 2 {
		t.Fatalf("fallback: %+v", fallback)
	}
	if _, err := store.Rollback(ctx, fallback.State.Generation, "must not retry the failed image"); !errors.Is(err, ErrNoFallback) {
		t.Fatalf("failed image became Previous: %v", err)
	}
}

func TestInitialPendingHasNoInventedFallbackAndCancelPreservesWatermark(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed boot", true: "canceled"}[cancel], func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			ref := stageFixture(t, store, "fixture-r2", 2)
			pending, err := store.Prepare(ctx, ref, 0, PrepareOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				state, err := store.CancelPending(ctx, pending.Generation)
				if err != nil || state.Active != nil || state.HighWatermark != 2 || state.LastFailure.Release != ref {
					t.Fatalf("initial cancel: %+v %v", state, err)
				}
			} else {
				selection, err := store.BeginBoot(ctx, "boot-1")
				if err != nil {
					t.Fatal(err)
				}
				selection.Image.Close()
				if _, err := store.Rollback(ctx, selection.State.Generation, "no known good release"); !errors.Is(err, ErrNoFallback) {
					t.Fatalf("invented rollback: %v", err)
				}
				result, err := store.BeginBoot(ctx, "boot-2")
				if !errors.Is(err, ErrNoFallback) || result.Image != nil || result.State.Active != nil || result.State.Pending != nil || result.State.HighWatermark != 2 {
					t.Fatalf("no-fallback failure: %+v %v", result, err)
				}
			}
			state := mustState(t, store)
			if _, err := store.Initialize(ctx, ref); err == nil {
				t.Fatal("failed initial candidate allowed initialization to reset watermark")
			}
			if _, err := store.Prepare(ctx, ref, state.Generation, PrepareOptions{}); !errors.Is(err, ErrDowngrade) {
				t.Fatalf("failed/canceled initial sequence reused: %v", err)
			}
			if _, err := store.BeginBoot(ctx, "recovery-boot"); !errors.Is(err, ErrNoActive) {
				t.Fatalf("missing active release returned success: %v", err)
			}
		})
	}
}

func TestExplicitHealthFailureFencesTokenAndRollsBack(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	if _, err := store.Prepare(ctx, second, 1, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	selection, err := store.BeginBoot(ctx, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	selection.Image.Close()
	if _, err := store.FailBoot(ctx, "boot-1", "incorrect", "readiness timed out"); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong health token: %v", err)
	}
	state, err := store.FailBoot(ctx, "boot-1", selection.State.Attempt.Token, "readiness timed out")
	if err != nil {
		t.Fatal(err)
	}
	if *state.Active != first || state.Previous != nil || state.LastFailure.Release != second || state.Operation != "boot-rollback" {
		t.Fatalf("health failure: %+v", state)
	}
}

func TestConfirmationCanceledAfterVerificationDoesNotCommit(t *testing.T) {
	store := newTestStore(t)
	initializeFixture(t, store)
	next := stageFixture(t, store, "fixture-r2", 2)
	if _, err := store.Prepare(context.Background(), next, 1, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	selection, err := store.BeginBoot(context.Background(), "boot-with-bounded-health")
	if err != nil {
		t.Fatal(err)
	}
	selection.Image.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := store.now
	// next() reads this clock after readState has finished re-verifying the
	// signed pairs. Cancel at that exact boundary without sleep-based races.
	store.now = func() time.Time {
		cancel()
		return clock()
	}
	_, err = store.Confirm(ctx, selection.State.Attempt.BootID, selection.State.Attempt.Token, next.Digest)
	store.now = clock
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expired decision window was accepted: %v", err)
	}
	if current := mustState(t, store); !reflect.DeepEqual(current, selection.State) {
		t.Fatalf("late confirmation changed durable trial state: %+v", current)
	}
}

func TestAtomicSelectorLinksMatchCompleteGeneration(t *testing.T) {
	store := newTestStore(t)
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	if _, err := store.Prepare(context.Background(), second, 1, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	generation, err := os.Readlink(filepath.Join(store.options.Root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	for name, ref := range map[string]Ref{"active": first, "pending": second} {
		link, err := os.Readlink(filepath.Join(store.options.Root, generation, name))
		if err != nil || link != "../../releases/"+ref.Digest {
			t.Fatalf("%s: %q %v", name, link, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(store.options.Root, generation, "previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected Previous: %v", err)
	}
}

func TestCorruptionAndMissingContentFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, s *Store, ref Ref)
	}{
		{"missing selector", func(t *testing.T, s *Store, r Ref) { mustRemove(t, filepath.Join(s.options.Root, "current")) }},
		{"escape selector", func(t *testing.T, s *Store, r Ref) {
			mustRemove(t, filepath.Join(s.options.Root, "current"))
			if err := os.Symlink("../escape", filepath.Join(s.options.Root, "current")); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed valid JSON", func(t *testing.T, s *Store, r Ref) {
			path := filepath.Join(s.options.Root, "current", "state.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte("\"high_watermark\":1"), []byte("\"high_watermark\":9"), 1)
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0400); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing ELF", func(t *testing.T, s *Store, r Ref) {
			dir := filepath.Join(s.options.Root, "releases", r.Digest)
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			mustRemove(t, filepath.Join(dir, "routerd-modules"))
		}},
		{"tampered ELF", func(t *testing.T, s *Store, r Ref) {
			path := filepath.Join(s.options.Root, "releases", r.Digest, "routerd")
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteAt([]byte("bad"), 4096)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0500); err != nil {
				t.Fatal(err)
			}
		}},
		{"ELF symlink", func(t *testing.T, s *Store, r Ref) {
			dir := filepath.Join(s.options.Root, "releases", r.Digest)
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "routerd")
			mustRemove(t, path)
			if err := os.Symlink("/bin/sh", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong active link", func(t *testing.T, s *Store, r Ref) {
			target, err := os.Readlink(filepath.Join(s.options.Root, "current"))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(s.options.Root, target)
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "active")
			mustRemove(t, path)
			if err := os.Symlink("../../releases/"+strings.Repeat("a", 64), path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ref := initializeFixture(t, store)
			tc.damage(t, store, ref)
			if _, err := store.Status(context.Background()); err == nil {
				t.Fatal("corrupt state returned success")
			}
			if _, err := store.BeginBoot(context.Background(), "boot-1"); err == nil {
				t.Fatal("corrupt state selected a boot image")
			}
			reopened, err := Open(store.options)
			if err == nil {
				defer reopened.Close()
				if _, err := reopened.Status(context.Background()); err == nil {
					t.Fatal("corrupt store was accepted or silently reinitialized after reopening")
				}
			}
		})
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRefusesSymlinkAncestorsPrivateViolationsAndHardLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "redirect")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(storeOptions(filepath.Join(link, "child"))); err == nil {
		s.Close()
		t.Fatal("symlink ancestor accepted")
	}
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(storeOptions(target)); err == nil {
		s.Close()
		t.Fatal("nonprivate store accepted")
	}
	store := newTestStore(t)
	ref := initializeFixture(t, store)
	artifact := filepath.Join(store.options.Root, "releases", ref.Digest, "routerd")
	if err := os.Link(artifact, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Status(context.Background()); err == nil {
		t.Fatal("hard-linked installed image accepted")
	}
}

func TestPruneProtectsSelectionAndReclaimsExplicitUnselectedVersions(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	third := stageFixture(t, store, "fixture-r3", 3)
	state, err := store.Prepare(ctx, second, 1, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []Ref{first, second} {
		if _, err := store.Prune(ctx, state.Generation, []string{ref.Digest}); err == nil {
			t.Fatal("prune removed selected release")
		}
	}
	if _, err := store.Prune(ctx, 0, []string{third.Digest}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale cleanup: %v", err)
	}
	result, err := store.Prune(ctx, state.Generation, []string{third.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedReleases) != 1 || result.RemovedReleases[0] != third.Digest || len(result.RemovedGenerations) == 0 {
		t.Fatalf("prune: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(store.options.Root, "releases", third.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unselected release was not removed")
	}
	if got := mustState(t, store); !reflect.DeepEqual(got, state) {
		t.Fatal("prune changed the journal or watermark")
	}
}

func TestCapacityIsExplicitAndExistingStageRemainsIdempotent(t *testing.T) {
	store := newTestStore(t)
	store.options.MaxStoredReleases = 2
	store.options.MaxGenerations = 4
	first := initializeFixture(t, store)
	second := stageFixture(t, store, "fixture-r2", 2)
	if _, err := store.Stage(context.Background(), bytes.NewReader(bundleFor(t, "fixture-r1", 1))); err != nil {
		t.Fatalf("idempotent stage at capacity: %v", err)
	}
	if _, err := store.Stage(context.Background(), bytes.NewReader(bundleFor(t, "fixture-r3", 3))); !errors.Is(err, ErrCapacity) {
		t.Fatalf("release capacity: %v", err)
	}
	if _, err := store.Prepare(context.Background(), second, 1, PrepareOptions{}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("prepare consumed space required to confirm or roll back: %v", err)
	}
	if current := mustState(t, store); current.Generation != 1 || current.Pending != nil || current.HighWatermark != 1 {
		t.Fatalf("failed capacity preflight changed selection: %+v", current)
	}
	if _, err := store.Prune(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(context.Background(), second, 1, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := store.BeginBoot(context.Background(), "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	selection.Image.Close()
	confirmed, err := store.Confirm(context.Background(), "boot-1", selection.State.Attempt.Token, second.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if *confirmed.Previous != first || prepared.HighWatermark != 2 {
		t.Fatal("capacity recovery changed release history")
	}
}

func TestPrepareReservesEveryAllowedBootAndFinalSelection(t *testing.T) {
	for _, outcome := range []string{"confirm", "fallback"} {
		t.Run(outcome, func(t *testing.T) {
			const attempts = 5
			ctx := context.Background()
			store := newTestStore(t)
			store.options.MaxGenerations = attempts + 3
			first := initializeFixture(t, store)
			second := stageFixture(t, store, "fixture-r2", 2)
			if _, err := store.Prepare(ctx, second, 1, PrepareOptions{MaxAttempts: attempts}); !errors.Is(err, ErrCapacity) {
				t.Fatalf("accepted a trial without its full generation budget: %v", err)
			}
			if _, err := store.Prune(ctx, 1, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Prepare(ctx, second, 1, PrepareOptions{MaxAttempts: attempts}); err != nil {
				t.Fatal(err)
			}
			var selection Selection
			var err error
			for i := 1; i <= attempts; i++ {
				selection, err = store.BeginBoot(ctx, fmt.Sprintf("kernel-boot-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				selection.Image.Close()
			}
			if outcome == "confirm" {
				if _, err := store.Confirm(ctx, selection.State.Attempt.BootID, selection.State.Attempt.Token, second.Digest); err != nil {
					t.Fatalf("reserved confirmation did not fit: %v", err)
				}
			} else {
				fallback, err := store.BeginBoot(ctx, "kernel-boot-fallback")
				if err != nil {
					t.Fatalf("reserved fallback did not fit: %v", err)
				}
				fallback.Image.Close()
				if !fallback.RolledBack || fallback.State.Active == nil || *fallback.State.Active != first {
					t.Fatalf("did not select known-good fallback: %+v", fallback)
				}
			}
			names, err := directoryNames(store.generations, store.options.MaxGenerations)
			if err != nil || len(names) != store.options.MaxGenerations {
				t.Fatalf("test did not reach exact final capacity: %d %v", len(names), err)
			}
		})
	}
}

// A currently executing sealed image remains independent of disk corruption.
// Uploading another candidate must not need that unrelated file to be readable.
func TestStageCandidateDoesNotRequireSelectedPayload(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	active := initializeFixture(t, store)
	path := filepath.Join(store.options.Root, "releases", active.Digest, "routerd")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt selected payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Status(ctx); err == nil {
		t.Fatal("corrupt selected image was accepted")
	}
	candidate := stageFixture(t, store, "fixture-r2", 2)
	if err := store.WithState(ctx, func(state State) error {
		if state.Active == nil || *state.Active != active || state.Pending != nil {
			t.Fatal("staging changed selection", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	image, err := store.openImage(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	image.Close()
}
