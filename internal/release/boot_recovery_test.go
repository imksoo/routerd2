// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func damageBootArtifact(t *testing.T, s *Store, ref Ref, missing bool) {
	t.Helper()
	dir := filepath.Join(s.options.Root, "releases", ref.Digest)
	path := filepath.Join(dir, "routerd-modules")
	if missing {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		mustRemove(t, path)
		return
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("damaged"), 4096)
	err = errors.Join(err, f.Close(), os.Chmod(path, 0500))
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenForBootSelectsFallbackBeforeOperationalOpen(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	target := stageFixture(t, s, "fixture-r2", 2)
	if _, err := s.Prepare(ctx, target, 1, PrepareOptions{MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	damageBootArtifact(t, s, target, false)
	options := s.options
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, selected, err := OpenForBoot(ctx, options, "new-kernel")
	if err != nil {
		t.Fatal("boot could not reach verified fallback:", err)
	}
	defer opened.Close()
	defer selected.Image.Close()
	if !selected.RolledBack || selected.Trial || selected.Image.Release != base || selected.State.LastFailure == nil || selected.State.LastFailure.Release != target {
		t.Fatal("wrong initial boot selection")
	}
	if got := mustState(t, opened); !reflect.DeepEqual(got, selected.State) {
		t.Fatal("boot result differs from committed selector")
	}
}

func TestBootRecoveryRefusesInvalidJournalFallbackAndTrialMetadata(t *testing.T) {
	for _, damage := range []string{"selector", "journal", "link", "fallback", "trial metadata"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			base := initializeFixture(t, s)
			target := stageFixture(t, s, "fixture-r2", 2)
			if _, err := s.Prepare(ctx, target, 1, PrepareOptions{MaxAttempts: 3}); err != nil {
				t.Fatal(err)
			}
			if damage == "trial metadata" {
				selected, err := s.BeginBoot(ctx, "initial-kernel")
				if err != nil {
					t.Fatal(err)
				}
				selected.Image.Close()
			}
			damageBootArtifact(t, s, target, false)
			switch damage {
			case "selector":
				mustRemove(t, filepath.Join(s.options.Root, "current"))
				if err := os.Symlink("../outside", filepath.Join(s.options.Root, "current")); err != nil {
					t.Fatal(err)
				}
			case "journal":
				path := filepath.Join(s.options.Root, "current", "state.json")
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{}`), 0400); err != nil {
					t.Fatal(err)
				}
			case "link":
				dir := filepath.Join(s.options.Root, "current")
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				mustRemove(t, filepath.Join(dir, "pending"))
				if err := os.Symlink("../../releases/"+strings.Repeat("f", 64), filepath.Join(dir, "pending")); err != nil {
					t.Fatal(err)
				}
			case "fallback":
				damageBootArtifact(t, s, base, false)
			case "trial metadata":
				path := filepath.Join(s.options.Root, "releases", target.Digest, "manifest.sig")
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, make([]byte, 64), 0400); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Readlink(filepath.Join(s.options.Root, "current"))
			if err != nil {
				t.Fatal(err)
			}
			opened, selected, err := OpenForBoot(ctx, s.options, "next-kernel")
			if opened != nil {
				opened.Close()
			}
			selected.Image.Close()
			if err == nil || selected.Image != nil {
				t.Fatal("invalid journal/fallback or unauthenticated trial metadata selected an executable")
			}
			if after, err := os.Readlink(filepath.Join(s.options.Root, "current")); err != nil || before != after {
				t.Fatal("refusal changed durable selector", err)
			}
		})
	}
}

func TestBootRecoveryPreservesPairedFallbackAndConfigurationFences(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	target := stageFixture(t, s, "fixture-r2", 2)
	prepared, err := s.Prepare(ctx, target, 1, pairOptions())
	if err != nil {
		t.Fatal(err)
	}
	trial, err := s.BeginBoot(ctx, "initial-kernel")
	if err != nil {
		t.Fatal(err)
	}
	trial.Image.Close()
	damageBootArtifact(t, s, target, false)
	selected, err := s.BeginBoot(ctx, "next-kernel")
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Image.Close()
	if selected.Image.Release != base || !sameStartup(selected.State.Startup, prepared.Pending.Fallback) || !sameStartup(selected.State.LastFailure.Startup, prepared.Pending.Startup) || selected.State.ConfigStoreID != prepared.ConfigStoreID || selected.State.ConfigHighWatermark != prepared.ConfigHighWatermark {
		t.Fatal("damaged trial rolled configuration authority back")
	}
}

func TestBootRecoveryCancellationBeforePublicationDoesNotSpendGeneration(t *testing.T) {
	s := newTestStore(t)
	initializeFixture(t, s)
	target := stageFixture(t, s, "fixture-r2", 2)
	if _, err := s.Prepare(context.Background(), target, 1, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	damageBootArtifact(t, s, target, false)
	before, err := os.Readlink(filepath.Join(s.options.Root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := s.now
	s.now = func() time.Time { cancel(); return clock() }
	_, err = s.BeginBoot(ctx, "canceled-recovery")
	s.now = clock
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expired recovery committed a new selection", err)
	}
	if after, err := os.Readlink(filepath.Join(s.options.Root, "current")); err != nil || before != after {
		t.Fatal("canceled recovery changed durable selector", err)
	}
}

func TestBootRecoversDamagedPendingPayloadWithoutExecutingIt(t *testing.T) {
	for _, trial := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(map[bool]string{false: "prepared", true: "trial"}[trial]+"/"+map[bool]string{false: "changed", true: "missing"}[missing], func(t *testing.T) {
				ctx := context.Background()
				s := newTestStore(t)
				base := initializeFixture(t, s)
				target := stageFixture(t, s, "fixture-r2", 2)
				if _, err := s.Prepare(ctx, target, 1, PrepareOptions{MaxAttempts: 3}); err != nil {
					t.Fatal(err)
				}
				if trial {
					selected, err := s.BeginBoot(ctx, "boot-initial")
					if err != nil {
						t.Fatal(err)
					}
					selected.Image.Close()
				}
				before := mustState(t, s)
				damageBootArtifact(t, s, target, missing)
				if opened, err := Open(s.options); err == nil {
					opened.Close()
					t.Fatal("ordinary operational open hid damaged candidate")
				}
				selected, err := s.BeginBoot(ctx, "boot-recovery")
				if err != nil {
					t.Fatal("damaged candidate prevented verified fallback:", err)
				}
				selected.Image.Close()
				state := selected.State
				if selected.Trial || !selected.RolledBack || selected.Image.Release != base || state.Active == nil || *state.Active != base || state.Pending != nil || state.Attempt != nil || state.Generation != before.Generation+1 || state.HighWatermark != before.HighWatermark || state.LastFailure == nil || state.LastFailure.Release != target {
					t.Fatal("fallback mixed selection, failure or durable fences:", state)
				}
				reopened, err := Open(s.options)
				if err != nil {
					t.Fatal("failed candidate remained a required reference:", err)
				}
				defer reopened.Close()
				again, err := reopened.BeginBoot(ctx, "boot-recovery")
				if err != nil {
					t.Fatal(err)
				}
				again.Image.Close()
				if again.Trial || again.RolledBack || !reflect.DeepEqual(again.State, state) {
					t.Fatal("repeated selection spent another generation or retried damaged candidate")
				}
			})
		}
	}
}
