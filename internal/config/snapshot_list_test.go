package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotDiscoveryRetainsUsableRecoveryWhenAnotherFileIsDamaged(t *testing.T) {
	store, current := snapshotFixture(t)
	empty, err := store.ListSnapshots()
	if err != nil || len(empty.Snapshots) != 0 || len(empty.Unreadable) != 0 {
		t.Fatal("new store listing", empty, err)
	}
	older, err := store.Snapshot(current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	current.Revision++
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}
	newer, err := store.Snapshot(current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := store.ListSnapshots()
	if err != nil || len(listing.Unreadable) != 0 || len(listing.Snapshots) != 2 || listing.Snapshots[0] != newer || listing.Snapshots[1] != older {
		t.Fatal("saved revisions are not discoverable in descending order", listing, err)
	}
	damaged := filepath.Join(store.Dir, "snapshots", newer.Digest+".json")
	if err := os.Chmod(damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, []byte("incomplete saved configuration\n"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(damaged, 0400); err != nil {
		t.Fatal(err)
	}
	listing, err = store.ListSnapshots()
	if err != nil || len(listing.Snapshots) != 1 || listing.Snapshots[0] != older || len(listing.Unreadable) != 1 || listing.Unreadable[0].File != newer.Digest+".json" || listing.Unreadable[0].Error == "" {
		t.Fatal("damaged save hid usable recovery or its error", listing, err)
	}
	if _, err := store.ResolveSnapshot(listing.Snapshots[0]); err != nil {
		t.Fatal("discovered recovery cannot be read", err)
	}
	after, err := store.Load()
	if err != nil || after.Revision != current.Revision || after.StoreID != current.StoreID {
		t.Fatal("discovery changed the current journal", err)
	}
}
