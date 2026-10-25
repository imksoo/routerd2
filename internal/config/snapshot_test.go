package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func snapshotFixture(t *testing.T) (*Store, Committed) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed.json")
	if err := AtomicJSON(seed, Document{SchemaVersion: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	s, c, err := Open(filepath.Join(t.TempDir(), "state"), seed)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestStoreIdentityAndImmutableSnapshot(t *testing.T) {
	s, c := snapshotFixture(t)
	other, elsewhere := snapshotFixture(t)
	if !validStoreID(c.StoreID) || c.StoreID == elsewhere.StoreID {
		t.Fatal("stores need distinct durable identities")
	}
	ref, err := s.Snapshot(c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if ref.StoreID != c.StoreID || ref.Revision != c.Revision || len(ref.Digest) != 64 {
		t.Fatal("incomplete snapshot identity", ref)
	}
	before, err := s.ResolveSnapshot(ref)
	if err != nil || !reflect.DeepEqual(before.Config, c.Config) {
		t.Fatal("snapshot cannot be resolved", err)
	}
	if _, err := other.ResolveSnapshot(ref); err == nil {
		t.Fatal("another store accepted a foreign identity")
	}
	c.Revision++
	c.Config.Resources = nil
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	reopened, got, err := Open(s.Dir, "/missing-seed")
	if err != nil || got.StoreID != ref.StoreID {
		t.Fatal("restart changed store identity", err)
	}
	resolved, err := reopened.ResolveSnapshot(ref)
	if err != nil || !reflect.DeepEqual(resolved, before) {
		t.Fatal("ordinary commit changed immutable snapshot", err)
	}
	if _, err := s.Snapshot(ref.Revision); err == nil {
		t.Fatal("stale snapshot request captured current revision")
	}
	if got, err := s.Snapshot(c.Revision); err != nil || got == ref {
		t.Fatal("new revision has no separate identity", err)
	}
}

func TestSnapshotRejectsUnconfirmedAndCorruptState(t *testing.T) {
	s, c := snapshotFixture(t)
	ref, err := s.Snapshot(c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "snapshots", ref.Digest+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("snapshot payload must be private and read only", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Even a valid document with altered provenance must fail its reference.
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["revision"] = 42
	changed, _ := json.Marshal(payload)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changed, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSnapshot(ref); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
	if _, err := s.Snapshot(c.Revision); err == nil {
		t.Fatal("capture repaired or overwrote corrupt existing snapshot")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, changed) {
		t.Fatal("capture overwrote corrupt evidence")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "config.json"), []byte(`{"revision":1,"config":{"schema_version":2,"resources":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(s.Dir, "/missing-seed"); err == nil {
		t.Fatal("journal without store identity was accepted")
	}
}

func TestSnapshotDoesNotFollowLinksOrAcceptPathReferences(t *testing.T) {
	for _, link := range []string{"directory", "payload"} {
		t.Run(link, func(t *testing.T) {
			s, c := snapshotFixture(t)
			ref, err := s.Snapshot(c.Revision)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(s.Dir, "snapshots")
			path := filepath.Join(root, ref.Digest+".json")
			if link == "directory" {
				if err := os.Rename(root, root+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"-moved", root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(path, path+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-moved", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ResolveSnapshot(ref); err == nil {
				t.Fatal("snapshot resolved through symlink")
			}
			if _, err := s.Snapshot(c.Revision); err == nil {
				t.Fatal("snapshot capture followed symlink")
			}
		})
	}
	s, c := snapshotFixture(t)
	ref, err := s.Snapshot(c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"../config", strings.Repeat("A", 64), strings.Repeat("a", 65), ""} {
		bad := ref
		bad.Digest = digest
		if _, err := s.ResolveSnapshot(bad); err == nil {
			t.Fatal("accepted unsafe digest reference")
		}
	}
	for _, mutate := range []func(*SnapshotRef){func(r *SnapshotRef) { r.Revision++ }, func(r *SnapshotRef) { r.StoreID = strings.Repeat("a", 32) }} {
		bad := ref
		mutate(&bad)
		if _, err := s.ResolveSnapshot(bad); err == nil {
			t.Fatal("accepted mismatched snapshot provenance")
		}
	}
}
