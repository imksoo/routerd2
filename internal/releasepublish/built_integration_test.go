// SPDX-License-Identifier: BSD-3-Clause
//go:build linux && integration

package releasepublish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/release"
)

func TestPublishBuiltRelease(t *testing.T) {
	dir, id := os.Getenv("ROUTERD2_RELEASE_BUILT_DIR"), os.Getenv("ROUTERD2_RELEASE_BUILT_ID")
	if dir == "" || id == "" {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("ROUTERD2_RELEASE_BUILT_DIR and ROUTERD2_RELEASE_BUILT_ID are required")
		}
		t.Skip("actual command builds are required")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			keyPath, public := testKey(t)
			path := filepath.Join(t.TempDir(), "release.tar")
			var stdout bytes.Buffer
			args := []string{"pack", "--private-key-file", keyPath, "--layout", testLayout(t, filepath.Join(dir, "linux-"+arch, "routerd"), filepath.Join(dir, "linux-"+arch, "routerd-modules"), filepath.Join(dir, "linux-"+arch, "routerd-logging")), "--release-id", id, "--sequence", "42", "--target-arch", arch, "--output", path}
			if err := Run(context.Background(), args, &stdout, io.Discard); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Path     string           `json:"path"`
				Digest   string           `json:"digest"`
				Manifest release.Manifest `json:"manifest"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "store")
			// The production store intentionally makes generations immutable.
			// Restore directory write permission only inside this test's private
			// temporary tree, before testing.TempDir performs its own cleanup.
			t.Cleanup(func() {
				if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() {
						return os.Chmod(path, 0700)
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
			store, err := release.Open(release.Options{Root: root, TargetArch: arch, TrustedKeys: map[string]ed25519.PublicKey{release.KeyID(public): public}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ref, err := store.StageFile(context.Background(), path)
			if err != nil || ref.Digest != result.Digest || ref.ReleaseID != id || ref.GOARCH != arch || result.Path != path {
				t.Fatalf("published CLI result and runtime verification differ: %+v %v", ref, err)
			}
			t.Logf("%s CLI bundle accepted by runtime: %s", arch, ref.Digest)
		})
	}
}
