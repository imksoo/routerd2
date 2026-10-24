//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	stateMountFlags = uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_RELATIME)
	stateMountData  = "errors=remount-ro,data=ordered,barrier=1"
	devicePoll      = 100 * time.Millisecond
)

var mountMu sync.Mutex

type deviceIdentity struct {
	major, minor uint32
	uuid         string
	errorDefault uint16
}

type blockDevice interface {
	Close() error
	fdPath() string
	identity() deviceIdentity
}

type directoryInfo struct {
	major, minor uint32
	uid, gid     uint32
	mode         uint32
	mountID      uint64
	fsType       int64
	fsFlags      uint64
}

type directory interface {
	Close() error
	fdPath() string
	inspect() (directoryInfo, error)
	empty() (bool, error)
	secureAndSync() error
}

// mountSystem is deliberately private. The public entry point always uses
// native syscalls; tests inject an inert backend and never mount on the host.
type mountSystem interface {
	pid() int
	euid() int
	now() time.Time
	wait(context.Context, time.Duration) error
	openDevice(string) (blockDevice, error)
	readMounts() ([]mountInfo, error)
	openTarget(string, bool) (directory, error)
	mount(source, target, filesystem string, flags uintptr, data string) error
}

// Mount makes an explicitly selected ext4 filesystem the state directory of
// appliance PID 1. The operation never formats a device, changes a foreign
// mount, or runs an external command. Device appearance is bounded by
// WaitTimeout; a blocked kernel filesystem or device I/O syscall cannot be
// interrupted safely by this API and needs a platform watchdog.
//
// Context cancellation is checked before starting a mount. Once mount succeeds,
// ownership checks, restrictive permissions, and directory fsync are completed
// even if ctx is canceled. An error with Status.Mounted=true requires startup to
// stop; this function does not guess whether it is safe to undo that mount.
func Mount(ctx context.Context, o Options) (Status, error) {
	return mountVolume(ctx, o, nativeSystem{})
}

func mountVolume(ctx context.Context, o Options, system mountSystem) (Status, error) {
	status := Status{Device: o.Device, Target: o.Target, FileSystem: "ext4"}
	if ctx == nil {
		return status, errors.New("state volume context is nil")
	}
	if err := o.Validate(); err != nil {
		return status, err
	}
	if system.pid() != 1 || system.euid() != 0 {
		return status, ErrForbidden
	}
	if !mountMu.TryLock() {
		return status, ErrBusy
	}
	defer mountMu.Unlock()
	device, err := waitDevice(ctx, o, system)
	if err != nil {
		return status, err
	}
	defer device.Close()
	identity := device.identity()
	status.DeviceMajor, status.DeviceMinor, status.UUID = identity.major, identity.minor, identity.uuid
	selected, err := readSelection(system, o.Target, identity)
	if err != nil {
		return status, err
	}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	target, err := system.openTarget(o.Target, selected.exact == nil)
	if err != nil {
		return status, fmt.Errorf("open state volume target: %w", err)
	}
	defer func() {
		if target != nil {
			target.Close()
		}
	}()
	info, err := target.inspect()
	if err != nil {
		return status, fmt.Errorf("inspect state volume target: %w", err)
	}
	if err := validateDirectory(info); err != nil {
		return status, err
	}
	if err := verifyCover(info, selected.cover); err != nil {
		return status, err
	}
	if selected.exact != nil {
		if err := verifyFilesystem(info, *selected.exact); err != nil {
			return status, err
		}
		status.Mounted, status.AlreadyMounted = true, true
	} else {
		empty, err := target.empty()
		if err != nil {
			return status, fmt.Errorf("inspect unmounted state directory: %w", err)
		}
		if !empty {
			return status, fmt.Errorf("%w: unmounted target is not empty", ErrConflict)
		}
		current, err := readSelection(system, o.Target, identity)
		if err != nil {
			return status, err
		}
		if current.exact != nil || current.cover.id != selected.cover.id {
			return status, fmt.Errorf("%w: mount topology changed before mount", ErrConflict)
		}
		// The pinned fd must still belong to the covering mount seen immediately
		// before the syscall. Source and target lookups use these private fds.
		info, err = target.inspect()
		if err != nil {
			return status, fmt.Errorf("recheck unmounted state directory: %w", err)
		}
		if err := verifyCover(info, current.cover); err != nil {
			return status, err
		}
		if err := ctx.Err(); err != nil {
			return status, err
		}
		if err := system.mount(device.fdPath(), target.fdPath(), "ext4", stateMountFlags, stateMountData); err != nil {
			return status, fmt.Errorf("mount state volume: %w", err)
		}
		status.Mounted = true
		// A pre-mount fd continues to refer to the covered directory. Open the
		// path again without following any symlinks to inspect the mounted root.
		target.Close()
		target = nil
		selected, err = readSelection(system, o.Target, identity)
		if err != nil {
			return status, fmt.Errorf("verify newly mounted state volume: %w", err)
		}
		if selected.exact == nil {
			return status, fmt.Errorf("%w: new mount is missing from mountinfo", ErrConflict)
		}
		target, err = system.openTarget(o.Target, false)
		if err != nil {
			return status, fmt.Errorf("reopen newly mounted state volume: %w", err)
		}
		info, err = target.inspect()
		if err != nil {
			return status, fmt.Errorf("inspect newly mounted state volume: %w", err)
		}
		if err := validateDirectory(info); err != nil {
			return status, err
		}
		if err := verifyFilesystem(info, *selected.exact); err != nil {
			return status, err
		}
	}
	status.MountID = selected.exact.id
	status.MountOptions = append([]string(nil), selected.exact.options...)
	status.SuperOptions = append([]string(nil), selected.exact.superOptions...)
	if err := target.secureAndSync(); err != nil {
		return status, fmt.Errorf("make state root private and durable: %w", err)
	}
	final, err := readSelection(system, o.Target, identity)
	if err != nil {
		return status, fmt.Errorf("final state volume verification: %w", err)
	}
	if final.exact == nil || final.exact.id != status.MountID {
		return status, fmt.Errorf("%w: mounted state root changed during setup", ErrConflict)
	}
	info, err = target.inspect()
	if err != nil {
		return status, fmt.Errorf("inspect secured state volume: %w", err)
	}
	if err := verifyFilesystem(info, *final.exact); err != nil {
		return status, err
	}
	if err := validateDirectory(info); err != nil {
		return status, err
	}
	if info.mode&07777 != 0700 {
		return status, fmt.Errorf("%w: state root mode is not 0700 after setup", ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	return status, nil
}

func waitDevice(ctx context.Context, o Options, system mountSystem) (blockDevice, error) {
	timeout := o.WaitTimeout
	if timeout == 0 {
		timeout = DefaultWaitTimeout
	}
	deadline := system.now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		device, err := system.openDevice(o.Device)
		if err == nil {
			return device, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENXIO) && !errors.Is(err, unix.ENODEV) {
			return nil, fmt.Errorf("open state volume device: %w", err)
		}
		remaining := deadline.Sub(system.now())
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: %s did not appear within %s", ErrDeviceTimeout, o.Device, timeout)
		}
		if remaining > devicePoll {
			remaining = devicePoll
		}
		if err := system.wait(ctx, remaining); err != nil {
			return nil, err
		}
	}
}

func readSelection(system mountSystem, target string, identity deviceIdentity) (selection, error) {
	entries, err := system.readMounts()
	if err != nil {
		return selection{}, fmt.Errorf("read state volume mount table: %w", err)
	}
	selected, err := selectMount(entries, target, identity.major, identity.minor)
	if err != nil {
		return selected, err
	}
	// ext4 omits an errors= value when it matches s_errors in the on-disk
	// superblock. An omitted errors=continue or errors=panic is still unsafe
	// for this state-volume contract; consult the selected device's default.
	if selected.exact != nil && !has(selected.exact.superOptions, "errors=remount-ro") && identity.errorDefault != 2 {
		return selected, fmt.Errorf("%w: effective ext4 error policy must be remount-ro", ErrConflict)
	}
	return selected, nil
}

func validateDirectory(info directoryInfo) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || info.uid != 0 || info.gid != 0 || info.mode&0022 != 0 {
		return fmt.Errorf("%w: state root must be a root-owned directory without group or other write access", ErrConflict)
	}
	return nil
}

func verifyCover(info directoryInfo, m mountInfo) error {
	if info.mountID != m.id || info.major != m.major || info.minor != m.minor {
		return fmt.Errorf("%w: target fd does not match its mountinfo identity", ErrConflict)
	}
	return nil
}

func verifyFilesystem(info directoryInfo, m mountInfo) error {
	if err := verifyCover(info, m); err != nil {
		return err
	}
	if info.fsType != unix.EXT4_SUPER_MAGIC || info.fsFlags&(unix.ST_NOSUID|unix.ST_NODEV) != unix.ST_NOSUID|unix.ST_NODEV || info.fsFlags&(unix.ST_RDONLY|unix.ST_NOEXEC) != 0 {
		return fmt.Errorf("%w: target statfs is not executable, writable ext4 with nosuid and nodev", ErrConflict)
	}
	return nil
}
