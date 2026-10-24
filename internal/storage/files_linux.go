//go:build linux

package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type nativeSystem struct{}

func (nativeSystem) pid() int       { return os.Getpid() }
func (nativeSystem) euid() int      { return os.Geteuid() }
func (nativeSystem) now() time.Time { return time.Now() }
func (nativeSystem) wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (nativeSystem) openDevice(path string) (blockDevice, error) {
	parent, err := openDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat selected device: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK || st.Uid != 0 || st.Mode&0002 != 0 || unix.Major(uint64(st.Rdev)) == 0 {
		return nil, errors.New("selected device must be a root-owned block node without other-write access")
	}
	readOnly, err := unix.IoctlGetInt(fd, unix.BLKROGET)
	if err != nil {
		return nil, fmt.Errorf("read block device flags: %w", err)
	}
	if readOnly != 0 {
		return nil, fmt.Errorf("%w: selected block device is read-only", ErrConflict)
	}
	super := make([]byte, ext4SuperblockSize)
	if _, err := f.ReadAt(super, 1024); err != nil {
		return nil, fmt.Errorf("read ext4 superblock: %w", err)
	}
	uuid, err := inspectSuperblock(super)
	if err != nil {
		return nil, err
	}
	ok = true
	return &nativeDevice{File: f, ident: deviceIdentity{major: unix.Major(uint64(st.Rdev)), minor: unix.Minor(uint64(st.Rdev)), uuid: uuid, errorDefault: binary.LittleEndian.Uint16(super[0x3c:])}}, nil
}

type nativeDevice struct {
	*os.File
	ident deviceIdentity
}

func (d *nativeDevice) fdPath() string           { return procFD(d.Fd()) }
func (d *nativeDevice) identity() deviceIdentity { return d.ident }

func (nativeSystem) readMounts() ([]mountInfo, error) {
	b, err := readProcFile("/proc/self/mountinfo", maxMountInfoBytes)
	if err != nil {
		return nil, err
	}
	return parseMountInfo(b)
}

func (nativeSystem) openTarget(path string, create bool) (directory, error) {
	f, err := openDirectory(path, create)
	if err != nil {
		return nil, err
	}
	return &nativeDirectory{File: f}, nil
}

func (nativeSystem) mount(source, target, filesystem string, flags uintptr, data string) error {
	return unix.Mount(source, target, filesystem, flags, data)
}

type nativeDirectory struct{ *os.File }

func (d *nativeDirectory) fdPath() string { return procFD(d.Fd()) }

func (d *nativeDirectory) inspect() (directoryInfo, error) {
	var info directoryInfo
	var st unix.Stat_t
	if err := unix.Fstat(int(d.Fd()), &st); err != nil {
		return info, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(d.Fd()), &fs); err != nil {
		return info, err
	}
	b, err := readProcFile("/proc/self/fdinfo/"+strconv.FormatUint(uint64(d.Fd()), 10), 4096)
	if err != nil {
		return info, err
	}
	mountID, err := parseFDMountID(b)
	if err != nil {
		return info, err
	}
	info = directoryInfo{major: unix.Major(uint64(st.Dev)), minor: unix.Minor(uint64(st.Dev)), uid: st.Uid, gid: st.Gid, mode: st.Mode, mountID: mountID, fsType: int64(fs.Type), fsFlags: uint64(fs.Flags)}
	return info, nil
}

func (d *nativeDirectory) empty() (bool, error) {
	names, err := d.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) == 0, nil
}

func (d *nativeDirectory) secureAndSync() error {
	if err := unix.Fchmod(int(d.Fd()), 0700); err != nil {
		return fmt.Errorf("chmod state root: %w", err)
	}
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync state root: %w", err)
	}
	return nil
}

func procFD(fd uintptr) string { return "/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10) }

// openDirectory anchors every component to an open parent fd. Root must own all
// components and no component may permit group or other writes. In particular,
// a sticky /tmp directory is intentionally unsuitable as a production target.
func openDirectory(path string, create bool) (*os.File, error) {
	if path != "/" {
		if err := validatePath(path); err != nil {
			return nil, err
		}
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
	if err := validateOwnedDirectory(current); err != nil {
		return nil, err
	}
	if path != "/" {
		for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			nextFD, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ENOENT) && create {
				if err = unix.Mkdirat(int(current.Fd()), component, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
					return nil, fmt.Errorf("create state directory component: %w", err)
				}
				if err := current.Sync(); err != nil {
					return nil, fmt.Errorf("fsync state directory parent: %w", err)
				}
				nextFD, err = unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
			if err != nil {
				return nil, err
			}
			next := os.NewFile(uintptr(nextFD), component)
			if err := validateOwnedDirectory(next); err != nil {
				next.Close()
				return nil, err
			}
			current.Close()
			current = next
		}
	}
	ok = true
	return current, nil
}

func validateOwnedDirectory(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
		return fmt.Errorf("%w: each path component must be a root-owned directory without group or other write access", ErrConflict)
	}
	return nil
}

func readProcFile(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return nil, err
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		return nil, errors.New("kernel metadata is not backed by procfs")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("kernel metadata exceeds size limit")
	}
	return b, nil
}

func parseFDMountID(data []byte) (uint64, error) {
	var id uint64
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "mnt_id:") {
			continue
		}
		if id != 0 {
			return 0, errors.New("fdinfo has duplicate mount IDs")
		}
		var err error
		if id, err = parsePositive(strings.TrimSpace(strings.TrimPrefix(line, "mnt_id:"))); err != nil {
			return 0, err
		}
	}
	if id == 0 {
		return 0, errors.New("fdinfo has no mount ID")
	}
	return id, nil
}
