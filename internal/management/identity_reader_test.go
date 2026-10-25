//go:build linux

package management

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostIdentityRejectsUnsafeExistingFile(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "readable", "parent symlink", "writable parent", "second key", "trailing data"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "host-key")
			if _, err := HostKey(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink", "hardlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(path+".original", path)
				} else {
					err = os.Link(path+".original", path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "readable":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "host-key")
			case "writable parent":
				if err := os.Chmod(dir, 0770); err != nil {
					t.Fatal(err)
				}
			case "second key":
				if err := os.WriteFile(path, append(raw, raw...), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing data":
				if err := os.WriteFile(path, append(raw, []byte("unexpected trailing content")...), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := HostKey(path); err == nil {
				t.Fatal("unsafe or ambiguous identity accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("refusal repaired or replaced key", err)
			}
		})
	}
}
