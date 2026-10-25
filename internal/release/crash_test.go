// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

var journalBoundaries = []string{
	"journal-state-synced", "journal-generation-synced", "journal-generation-published",
	"journal-parent-synced", "journal-selector-created", "journal-selector-replaced", "journal-root-synced",
}

func TestJournalErrorBoundariesNeverMixStateAndSymlinks(t *testing.T) {
	for _, point := range journalBoundaries {
		t.Run(point, func(t *testing.T) {
			store := newTestStore(t)
			first := initializeFixture(t, store)
			second := stageFixture(t, store, "fixture-r2", 2)
			before := mustState(t, store)
			injected := errors.New("injected storage failure")
			store.fault = func(at string) error {
				if at == point {
					return injected
				}
				return nil
			}
			_, err := store.Prepare(context.Background(), second, before.Generation, PrepareOptions{})
			store.fault = nil
			if !errors.Is(err, injected) {
				t.Fatalf("injection did not reach %s: %v", point, err)
			}
			after := mustState(t, store)
			published := point == "journal-selector-replaced" || point == "journal-root-synced"
			if published {
				if !errors.Is(err, ErrUncertain) || after.Generation != before.Generation+1 || after.Pending == nil || after.Pending.Release != second || *after.Active != first {
					t.Fatalf("uncertain commit is not observable as one generation: %+v %v", after, err)
				}
			} else {
				if errors.Is(err, ErrUncertain) || !reflect.DeepEqual(after, before) {
					t.Fatalf("pre-publication failure changed state: %+v %v", after, err)
				}
			}
		})
	}
}

// A subprocess uses os.Exit at the actual durable-write boundaries, so the
// cleanup defers do not run. This is process-crash recovery, not a claim that a
// userspace test can simulate power failure in every disk/controller/filesystem.
func TestProcessCrashAtJournalBoundaries(t *testing.T) {
	for _, point := range journalBoundaries {
		t.Run(point, func(t *testing.T) {
			store := newTestStore(t)
			first := initializeFixture(t, store)
			second := stageFixture(t, store, "fixture-r2", 2)
			crashChild(t, store.options.Root, point, "prepare", second, "")
			reopened, err := Open(store.options)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			state := mustState(t, reopened)
			published := point == "journal-selector-replaced" || point == "journal-root-synced"
			if *state.Active != first {
				t.Fatal("crash changed confirmed active before boot")
			}
			if published {
				if state.Generation != 2 || state.HighWatermark != 2 || state.Pending == nil || state.Pending.Release != second {
					t.Fatalf("published transaction lost or mixed: %+v", state)
				}
			} else {
				if state.Generation != 1 || state.HighWatermark != 1 || state.Pending != nil {
					t.Fatalf("unpublished transaction became active: %+v", state)
				}
			}
			if _, err := reopened.Prune(context.Background(), state.Generation, nil); err != nil {
				t.Fatalf("recover orphan transaction: %v", err)
			}
			if current := mustState(t, reopened); !reflect.DeepEqual(current, state) {
				t.Fatal("orphan cleanup changed committed state")
			}
		})
	}
}

func TestProcessCrashAtStagingBoundariesLeavesNoSelectablePartialImage(t *testing.T) {
	points := []string{"stage-signature-verified", "stage-routerd-synced", "stage-routerd-modules-synced", "stage-directory-synced", "stage-directory-published"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			store := newTestStore(t)
			first := initializeFixture(t, store)
			bundle := bundleFor(t, "fixture-r2", 2)
			path := filepath.Join(t.TempDir(), "update.tar")
			if err := os.WriteFile(path, bundle, 0600); err != nil {
				t.Fatal(err)
			}
			crashChild(t, store.options.Root, point, "stage", Ref{}, path)
			reopened, err := Open(store.options)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			state := mustState(t, reopened)
			if state.Generation != 1 || state.Pending != nil || *state.Active != first {
				t.Fatalf("staging crash changed selector: %+v", state)
			}
			if _, err := reopened.Prune(context.Background(), state.Generation, nil); err != nil {
				t.Fatal(err)
			}
			second, err := reopened.Stage(context.Background(), bytes.NewReader(bundle))
			if err != nil {
				t.Fatalf("retry crashed stage: %v", err)
			}
			if second.ReleaseID != "fixture-r2" {
				t.Fatal("wrong retried release")
			}
			if _, err := reopened.Prepare(context.Background(), second, state.Generation, PrepareOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func crashChild(t *testing.T, root, point, operation string, ref Ref, bundle string) {
	t.Helper()
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestReleaseCrashHelper$")
	command.Env = append(os.Environ(), "ROUTERD2_RELEASE_CRASH_ROOT="+root, "ROUTERD2_RELEASE_CRASH_POINT="+point, "ROUTERD2_RELEASE_CRASH_OPERATION="+operation, "ROUTERD2_RELEASE_CRASH_REF="+string(encoded), "ROUTERD2_RELEASE_CRASH_BUNDLE="+bundle)
	log, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 99 {
		t.Fatalf("crash helper did not exit at %s: %v\n%s", point, err, log)
	}
}

func TestReleaseCrashHelper(t *testing.T) {
	root := os.Getenv("ROUTERD2_RELEASE_CRASH_ROOT")
	if root == "" {
		return
	}
	var store *Store
	var err error
	if os.Getenv("ROUTERD2_RELEASE_CRASH_OPERATION") == "boot-recovery" {
		// Only the test helper opens raw pinned handles so it can install its
		// process-exit hook before the complete BeginBoot transaction.
		store, err = openStoreHandles(storeOptions(root))
	} else {
		store, err = Open(storeOptions(root))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	point := os.Getenv("ROUTERD2_RELEASE_CRASH_POINT")
	store.fault = func(at string) error {
		if at == point {
			os.Exit(99)
		}
		return nil
	}
	switch os.Getenv("ROUTERD2_RELEASE_CRASH_OPERATION") {
	case "prepare":
		var ref Ref
		if err := json.Unmarshal([]byte(os.Getenv("ROUTERD2_RELEASE_CRASH_REF")), &ref); err != nil {
			t.Fatal(err)
		}
		state := mustState(t, store)
		_, err = store.Prepare(context.Background(), ref, state.Generation, PrepareOptions{})
	case "stage":
		_, err = store.StageFile(context.Background(), os.Getenv("ROUTERD2_RELEASE_CRASH_BUNDLE"))
	case "boot-recovery":
		var selected Selection
		selected, err = store.BeginBoot(context.Background(), "crashed-recovery")
		selected.Image.Close()
	default:
		t.Fatal("invalid crash helper operation")
	}
	t.Fatalf("crash boundary not reached: %v", err)
}
