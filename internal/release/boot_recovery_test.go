// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
