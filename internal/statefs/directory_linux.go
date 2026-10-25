//go:build linux

// Package statefs opens protected persistent state without following links.
package statefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenDirectory pins each trusted directory component. Read-only callers pass
// false; initialization may create 0700 directories and synchronize parents.
func OpenDirectory(path string, create bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return nil, errors.New("protected state directory must be a clean absolute path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	ok := false
	defer func() {
		if !ok {
			current.Close()
		}
	}()
	check := func(f *os.File) error {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}
		stickyRoot := st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 && st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 && !stickyRoot {
			return errors.New("protected state path has an untrusted or writable directory")
		}
		return nil
	}
	if err := check(current); err != nil {
		return nil, err
	}
	if path != "/" {
		for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			fd, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ENOENT) && create {
				if err := unix.Mkdirat(int(current.Fd()), component, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
					return nil, err
				}
				fd, err = unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
			if err != nil {
				return nil, err
			}
			next := os.NewFile(uintptr(fd), component)
			if err := check(next); err != nil {
				next.Close()
				return nil, err
			}
			if create {
				if err := errors.Join(next.Sync(), current.Sync()); err != nil {
					next.Close()
					return nil, err
				}
			}
			current.Close()
			current = next
		}
	}
	ok = true
	return current, nil
}

// OpenParent accepts a clean absolute non-root file path. It never opens the
// final leaf and leaves its format-specific checks to the caller.
func OpenParent(path string, create bool) (*os.File, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return nil, "", errors.New("protected state file needs a clean absolute path")
	}
	dir, err := OpenDirectory(filepath.Dir(path), create)
	return dir, filepath.Base(path), err
}
