//go:build linux

package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Deliberately exclude atime: reading is allowed, creating files, changing
// permissions, replacing inodes and publishing/fsyncing an adoption are not.
func inspectionTree(t *testing.T, dir string) map[string]any {
	t.Helper()
	result := map[string]any{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var content [32]byte
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content = sha256.Sum256(raw)
		}
		st := info.Sys().(*syscall.Stat_t)
		result[path] = []any{info.Mode(), info.Size(), info.ModTime(), st.Ino, content}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStartupInspectionChecksRealCandidateAndRecoveryWithoutWriting(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before adoption", true: "after adoption"}[consumed], func(t *testing.T) {
			s, baseline, candidate, recovery := recoveryFixture(t)
			if consumed {
				if _, err := s.AdoptStartup(candidate); err != nil {
					t.Fatal(err)
				}
			}
			before := inspectionTree(t, s.Dir)
			s.checkpoint = func(string) error { t.Fatal("inspection published a configuration record"); return nil }
			check, err := s.InspectStartup(recovery)
			if err != nil || check.AlreadyAdopted || check.Recovered != consumed || !reflect.DeepEqual(check.Snapshot.Config, baseline.Config) || check.Snapshot.Version != SnapshotVersion || check.Current.StoreID != baseline.StoreID {
				t.Fatalf("wrong inspection branch: %+v %v", check, err)
			}
			if !reflect.DeepEqual(before, inspectionTree(t, s.Dir)) {
				t.Fatal("inspection changed persistent state")
			}
			s.checkpoint = nil
			adopted, err := s.AdoptStartup(recovery)
			if err != nil || !reflect.DeepEqual(adopted.Config, check.Snapshot.Config) || adopted.Startup.Recovered != check.Recovered || adopted.Revision != check.Current.Revision+1 {
				t.Fatal("inspection and adoption disagree", err)
			}
		})
	}
}

func TestStartupInspectionPreservesConsumedReceiptWithoutRereadingSnapshot(t *testing.T) {
	s, _, selection := startupFixture(t)
	first, err := s.InspectStartup(selection)
	if err != nil || first.AlreadyAdopted || first.Recovered || first.Snapshot.Revision != selection.Snapshot.Revision {
		t.Fatal("candidate inspection", err)
	}
	c, err := s.AdoptStartup(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.Dir, "snapshots")); err != nil {
		t.Fatal(err)
	}
	c.Revision++
	c.LastRequestID = "later-trial"
	c.Confirmation = &Confirmation{Previous: c.Config, PreviousRevision: c.Revision - 1, RequestID: c.LastRequestID, Deadline: time.Now().Add(time.Minute).UTC()}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	before := inspectionTree(t, s.Dir)
	got, err := s.InspectStartup(selection)
	if err != nil || !got.AlreadyAdopted || !reflect.DeepEqual(got.Current, c) || got.Snapshot.Version != 0 {
		t.Fatal("consumed receipt required deleted input or erased later trial", err)
	}
	if !reflect.DeepEqual(before, inspectionTree(t, s.Dir)) {
		t.Fatal("replay inspection changed state")
	}
}

func TestStartupInspectionRefusesWrongStateAndMissingInputs(t *testing.T) {
	for _, kind := range []string{"revision", "generation", "store", "confirmation", "capacity", "baseline", "missing snapshot", "corrupt snapshot"} {
		t.Run(kind, func(t *testing.T) {
			s, c, candidate, recovery := recoveryFixture(t)
			selection := candidate
			want := ErrStartupConflict
			switch kind {
			case "revision":
				selection.ExpectedRevision++
			case "generation":
				var err error
				c, err = s.AdoptStartup(candidate)
				if err != nil {
					t.Fatal(err)
				}
				selection.Generation--
				selection.ExpectedRevision = c.Revision
			case "store":
				selection.Snapshot.StoreID = strings.Repeat("e", 32)
			case "confirmation":
				c.LastRequestID = "trial"
				c.Confirmation = &Confirmation{Previous: c.Config, PreviousRevision: 1, RequestID: "trial", Deadline: time.Now().Add(time.Minute).UTC()}
				if err := s.Save(c); err != nil {
					t.Fatal(err)
				}
			case "capacity":
				for i := 0; i < MaxEventOutbox; i++ {
					c.EventOutbox = append(c.EventOutbox, ChangeEvent{ID: fmt.Sprintf("%032x", i+1), Type: "committed", Revision: c.Revision, ObservedAt: time.Now().UTC()})
				}
				if err := s.Save(c); err != nil {
					t.Fatal(err)
				}
				want = ErrStartupCapacity
			case "baseline":
				selection = recovery
				c.Config = Document{SchemaVersion: SchemaVersion}
				if err := s.Save(c); err != nil {
					t.Fatal(err)
				}
			case "missing snapshot":
				if err := os.Remove(filepath.Join(s.Dir, "snapshots", candidate.Snapshot.Digest+".json")); err != nil {
					t.Fatal(err)
				}
				want = os.ErrNotExist
			case "corrupt snapshot":
				path := filepath.Join(s.Dir, "snapshots", candidate.Snapshot.Digest+".json")
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0400); err != nil {
					t.Fatal(err)
				}
				want = nil
			}
			before := inspectionTree(t, s.Dir)
			if _, err := s.InspectStartup(selection); err == nil || want != nil && !errors.Is(err, want) {
				t.Fatal("wrong refusal", err)
			}
			if !reflect.DeepEqual(before, inspectionTree(t, s.Dir)) {
				t.Fatal("refused inspection changed state")
			}
		})
	}
}
