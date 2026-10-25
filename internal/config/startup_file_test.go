//go:build linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStartupAdoptionRejectsUnsafeAuthoritativeJournal(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory symlink", "readable journal", "writable directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			s, _, selection := startupFixture(t)
			path := filepath.Join(s.Dir, "config.json")
			switch kind {
			case "symlink", "hardlink", "fifo":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(path+"-original", path)
				case "hardlink":
					err = os.Link(path+"-original", path)
				case "fifo":
					err = unix.Mkfifo(path, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				if err := os.Rename(s.Dir, s.Dir+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(s.Dir+"-original", s.Dir); err != nil {
					t.Fatal(err)
				}
			case "readable journal":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "writable directory":
				if err := os.Chmod(s.Dir, 0770); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AdoptStartup(selection); err == nil {
				t.Fatal("unsafe journal was used as startup authority")
			}
			if _, err := s.InspectStartup(selection); err == nil {
				t.Fatal("unsafe journal accepted by inspection")
			}
		})
	}
}
