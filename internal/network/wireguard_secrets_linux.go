//go:build linux

package network

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/sys/unix"
)

func privateFileOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// All paths are walked from pinned directory descriptors. A root-owned sticky
// directory is a safe ancestor for private temporary/test directories, but an
// ordinary group/other-writable ancestor is not. Root and the worker UID are
// trusted owners of ancestors; the final secret must belong to the worker UID.
func openWireGuardSecretDirectory(path string) (*os.File, error) {
	return statefs.OpenDirectory(path, false)
}

// create is reserved for journal initialization. Read-only state inspection and
// secret resolution never create or synchronize directories.

func openWireGuardSecretParent(path string) (*os.File, string, error) {
	if !secretFilePath(path) || path == "/" {
		return nil, "", errors.New("WireGuard secret file must be a clean absolute non-root path")
	}
	parent, err := openWireGuardSecretDirectory(filepath.Dir(path))
	return parent, filepath.Base(path), err
}

func openWireGuardSecretFile(path string) (*os.File, error) {
	parent, name, err := openWireGuardSecretParent(path)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func prepareWireGuardSecretDirectory(path string) error {
	parent, name, err := openWireGuardSecretParent(path)
	if err != nil {
		return err
	}
	defer parent.Close()
	created := true
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		created = false
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), path)
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !privateFileOwner(info) {
		return errors.New("WireGuard secret store must be a worker-owned 0700 directory")
	}
	if created {
		return errors.Join(dir.Sync(), parent.Sync())
	}
	return nil
}

// A final name becomes visible only after complete contents are fsynced. A
// crash before publication can leave a private, randomly named temporary file;
// readers never consider it a source/snapshot and a retry uses another name.
// Existing files are never removed, repaired, or replaced, even on corruption.
// after is a private fault-injection hook; production callers always pass nil.
func publishWireGuardSecretFile(path string, data []byte, after func(string) error) error {
	parent, name, err := openWireGuardSecretParent(path)
	if err != nil {
		return err
	}
	defer parent.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("WireGuard secret temporary name generation failed")
	}
	temporary := ".wireguard-secret-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(int(parent.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temporary)
	defer f.Close()
	defer unix.Unlinkat(int(parent.Fd()), temporary, 0)
	checkpoint := func(stage string) error {
		if after != nil {
			return after(stage)
		}
		return nil
	}
	if err := checkpoint("created"); err != nil {
		return err
	}
	n, err := f.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := checkpoint("written"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := checkpoint("file-synced"); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(int(parent.Fd()), temporary, int(parent.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			// A prior attempt might have died after publishing but before syncing
			// the parent. Reassert durability before callers validate/reuse it.
			if syncErr := parent.Sync(); syncErr != nil {
				return fmt.Errorf("sync existing WireGuard secret directory: %w", syncErr)
			}
		}
		return err
	}
	if err := checkpoint("published"); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return checkpoint("directory-synced")
}
