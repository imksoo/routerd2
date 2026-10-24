//go:build linux

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFDMountIDStrictParsing(t *testing.T) {
	id, err := parseFDMountID([]byte("pos:\t0\nflags:\t0100000\nmnt_id:\t42\nino:\t8\n"))
	if err != nil || id != 42 {
		t.Fatalf("fd mount ID: %d %v", id, err)
	}
	for _, value := range []string{"", "mnt_id: 0\n", "mnt_id: -1\n", "mnt_id: +1\n", "mnt_id: x\n", "mnt_id: 18446744073709551616\n", "mnt_id: 1\nmnt_id: 2\n"} {
		if _, err := parseFDMountID([]byte(value)); err == nil {
			t.Fatalf("accepted malformed fdinfo %q", value)
		}
	}
}

func TestNativePathWalkRejectsUnsafeAncestor(t *testing.T) {
	// This is ordinary file I/O only: no device nodes and no mount syscall.
	unsafe := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafe, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(unsafe, "state")
	f, err := openDirectory(path, true)
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Fatal("accepted writable ancestor as persistent state path")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unsafe traversal created the target: %v", err)
	}
}

func TestNativeDirectoryOwnershipAndSync(t *testing.T) {
	path := t.TempDir()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if os.Geteuid() != 0 {
		if err := validateOwnedDirectory(f); !errors.Is(err, ErrConflict) {
			t.Fatalf("accepted non-root directory: %v", err)
		}
		return
	}
	if err := validateOwnedDirectory(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(0777); err != nil {
		t.Fatal(err)
	}
	if err := validateOwnedDirectory(f); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted writable directory: %v", err)
	}
	d := nativeDirectory{File: f}
	if err := d.secureAndSync(); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("secured directory: %v %v", st, err)
	}
	if empty, err := d.empty(); err != nil || !empty {
		t.Fatalf("empty directory: %v %v", empty, err)
	}
}

func TestNativeDeviceRejectsCharacterNodeWithoutMounting(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native root-owned device path inspection")
	}
	device, err := (nativeSystem{}).openDevice("/dev/null")
	if device != nil {
		device.Close()
	}
	if err == nil {
		t.Fatal("accepted character device as ext4 volume")
	}
	// /proc/self is an intentional kernel symlink. Reject it both as the
	// final component and in the middle of a target path; the metadata reader
	// deliberately has a separate, procfs-verified access path.
	for _, path := range []string{"/proc/self", "/proc/self/fd"} {
		if directory, err := openDirectory(path, false); err == nil {
			directory.Close()
			t.Fatalf("target path walker followed a symlink in %q", path)
		}
	}
}

func TestNativeKernelMetadataReadOnly(t *testing.T) {
	entries, err := (nativeSystem{}).readMounts()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := (&nativeDirectory{File: f}).inspect()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range entries {
		if m.id == info.mountID {
			if err := verifyCover(info, m); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found || info.mode&unix.S_IFMT != unix.S_IFDIR {
		t.Fatalf("real fd mount identity not in mount table: %#v", info)
	}
}
