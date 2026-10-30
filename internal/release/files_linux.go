// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var generationName = regexp.MustCompile("^g[0-9]{20}-[0-9a-f]{64}-[0-9a-f]{32}$")

func nonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func checkPrivate(f *os.File, directory bool, limit int64) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 {
		return errors.New("release objects must be private and owned by the current effective UID")
	}
	if directory {
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.New("release path is not a directory")
		}
	} else {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
			return errors.New("release file must be a regular file with exactly one link")
		}
		if stat.Size < 0 || limit >= 0 && stat.Size > limit {
			return errors.New("release file exceeds its size bound")
		}
	}
	return nil
}

func openDirectory(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err := checkPrivate(f, true, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func makeDirectory(parent *os.File, name string) (*os.File, error) {
	err := unix.Mkdirat(int(parent.Fd()), name, 0700)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	if err == nil {
		if err := parent.Sync(); err != nil {
			return nil, err
		}
	}
	return openDirectory(parent, name)
}

func openRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("release root must be a clean absolute non-root path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(int(current.Fd()), part, 0700); err == nil || errors.Is(err, unix.EEXIST) {
				if err = current.Sync(); err == nil {
					next, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				}
			}
		}
		current.Close()
		if err != nil {
			return nil, fmt.Errorf("release root component: %w", err)
		}
		current = os.NewFile(uintptr(next), part)
	}
	if err := checkPrivate(current, true, 0); err != nil {
		current.Close()
		return nil, err
	}
	return current, nil
}

func openRegular(dir *os.File, name string, flags int, mode uint32, max int64) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err := checkPrivate(f, false, max); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readFile(dir *os.File, name string, max int64) ([]byte, error) {
	f, err := openRegular(dir, name, unix.O_RDONLY, 0, max)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("release file grew beyond its size bound")
	}
	return b, nil
}

func writeFile(dir *os.File, name string, data []byte, mode uint32) error {
	f, err := openRegular(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600, -1)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(os.FileMode(mode)); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func readLink(dir *os.File, name string) (string, error) {
	var b [512]byte
	n, err := unix.Readlinkat(int(dir.Fd()), name, b[:])
	if err != nil {
		return "", err
	}
	if n == len(b) {
		return "", errors.New("release symlink target exceeds bound")
	}
	return string(b[:n]), nil
}

func directoryNames(dir *os.File, limit int) ([]string, error) {
	// A separate open file description avoids sharing a mutable directory offset.
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "directory-list")
	defer f.Close()
	names, err := f.Readdirnames(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > limit {
		return nil, ErrCapacity
	}
	return names, nil
}

// removeFlatDirectory never follows links or recurses into arbitrary content.
// It is used only for private temporary or verified store-owned directories.
func removeFlatDirectory(parent *os.File, name string, allowed map[string]bool) error {
	dir, err := openDirectory(parent, name)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := directoryNames(dir, len(allowed))
	if err != nil {
		return err
	}
	for _, child := range names {
		if !allowed[child] {
			return errors.New("unexpected entry in release directory; refusing removal")
		}
	}
	if err = dir.Chmod(0700); err != nil {
		return err
	}
	manifest := false
	for _, child := range names {
		if child == "manifest.json" {
			manifest = true
			continue
		}
		if err = unix.Unlinkat(int(dir.Fd()), child, 0); err != nil {
			return err
		}
	}
	// Cleanup reconstructs allowed artifact names from the manifest. Keep it
	// until all other deletions are durable so an interrupted prune can resume.
	if err = dir.Sync(); err != nil {
		return err
	}
	if manifest {
		if err = unix.Unlinkat(int(dir.Fd()), "manifest.json", 0); err != nil {
			return err
		}
		if err = dir.Sync(); err != nil {
			return err
		}
	}
	if err = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}

func (s *Store) checkpoint(name string) error {
	if s.fault != nil {
		return s.fault(name)
	}
	return nil
}

func (s *Store) checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil release context")
	}
	return ctx.Err()
}
