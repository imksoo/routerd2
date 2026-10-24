//go:build linux

package network

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWireGuardSecretPublicationCrash(t *testing.T) {
	const stageEnv = "ROUTERD2_WG_SECRET_CRASH_STAGE"
	const pathEnv = "ROUTERD2_WG_SECRET_CRASH_PATH"
	key := fixtureWireGuardKey(31)
	if stage := os.Getenv(stageEnv); stage != "" {
		// This child operates only on its parent's newly created private test
		// directory. os.Exit deliberately bypasses cleanup defers.
		path := os.Getenv(pathEnv)
		err := publishWireGuardSecretFile(path, key[:], func(at string) error {
			if at == stage {
				os.Exit(73)
			}
			return nil
		})
		t.Fatalf("crash point was not reached: %v", err)
	}
	for _, stage := range []string{"created", "written", "file-synced", "published", "directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, wireGuardSecretID(key))
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(exe, "-test.run=^TestWireGuardSecretPublicationCrash$", "-test.count=1")
			child.Env = append(os.Environ(), stageEnv+"="+stage, pathEnv+"="+path)
			output, err := child.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash child: %v\n%s", err, output)
			}
			published := stage == "published" || stage == "directory-synced"
			data, err := os.ReadFile(path)
			if published {
				if err != nil || !bytes.Equal(data, key[:]) {
					t.Fatal("published secret was incomplete", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a final secret name appeared before durable publication", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			orphans := map[string]bool{}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".wireguard-secret-") {
					info, err := entry.Info()
					if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
						t.Fatal("crash orphan is not private", err)
					}
					orphans[entry.Name()] = true
				}
			}
			if !published && len(orphans) != 1 {
				t.Fatalf("expected one ignored crash orphan, got %d", len(orphans))
			}
			err = publishWireGuardSecretFile(path, key[:], nil)
			if published && !errors.Is(err, os.ErrExist) || !published && err != nil {
				t.Fatalf("publication could not recover after crash: %v", err)
			}
			data, err = readPrivateRegularFile(path, 32)
			if err != nil || !bytes.Equal(data, key[:]) {
				t.Fatal("retry did not preserve complete immutable contents", err)
			}
			for orphan := range orphans {
				if _, err := os.Lstat(filepath.Join(dir, orphan)); err != nil {
					t.Fatal("retry removed an unrelated preexisting temporary file", err)
				}
			}
		})
	}
}

func TestWireGuardSecretPublicationNeverOverwritesOrFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	key := fixtureWireGuardKey(31)
	file := filepath.Join(dir, "key")
	if err := publishWireGuardSecretFile(file, key[:], nil); err != nil {
		t.Fatal(err)
	}
	other := fixtureWireGuardKey(99)
	if err := publishWireGuardSecretFile(file, other[:], nil); !errors.Is(err, os.ErrExist) {
		t.Fatal("immutable overwrite did not fail", err)
	}
	for _, path := range []string{filepath.Join(dir, "final-link"), filepath.Join(dir, "directory-link", "key")} {
		if strings.Contains(path, "directory-link") {
			if err := os.Symlink(dir, filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Symlink(file, path); err != nil {
			t.Fatal(err)
		}
		if _, err := readPrivateRegularFile(path, 32); err == nil {
			t.Fatal("secret reader followed a symlink")
		}
		if err := publishWireGuardSecretFile(path, other[:], nil); err == nil {
			t.Fatal("secret publisher followed a symlink")
		}
	}
	data, err := readPrivateRegularFile(file, 32)
	if err != nil || !bytes.Equal(data, key[:]) {
		t.Fatal("immutable contents changed", err)
	}
	// O_NONBLOCK prevents a regular-file replacement with a FIFO from blocking
	// before fstat rejects its type. This creates no block device or mount.
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateRegularFile(fifo, 32); err == nil {
		t.Fatal("secret reader accepted a FIFO")
	}
}
