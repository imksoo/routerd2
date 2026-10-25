//go:build linux

package events

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingStoreRejectsChangedRequiredDDLBeforeWritableOpen(t *testing.T) {
	for _, change := range []string{"ALTER TABLE events ADD COLUMN foreign_owner TEXT", "DROP INDEX pending_work"} {
		t.Run(change, func(t *testing.T) {
			opts := Options{Directory: filepath.Join(t.TempDir(), "events")}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(opts.Directory, "journal.sqlite3")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if opened, err := OpenExisting(opts); err == nil {
				opened.Close()
				t.Fatal("changed required schema accepted despite unchanged version")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejection modified database", err)
			}
		})
	}
}
