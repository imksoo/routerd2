// Package storage mounts an explicitly selected, existing ext4 state volume.
// It does not discover, format, repair, or unmount disks.
package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultWaitTimeout = 10 * time.Second
	MaxWaitTimeout     = 5 * time.Minute
	maxPathBytes       = 4095
)

var (
	ErrForbidden     = errors.New("state volume mount requires appliance PID 1 running as root")
	ErrBusy          = errors.New("state volume mount is already in progress")
	ErrDeviceTimeout = errors.New("state volume device wait expired")
	ErrConflict      = errors.New("state volume mount conflicts with existing filesystem state")
	ErrFilesystem    = errors.New("state volume is not a supported ext4 filesystem")
)

// Options deliberately has no filesystem type, format, mount option, or device
// discovery setting. Target is the appliance's configured state directory.
type Options struct {
	Platform    string
	Device      string
	Target      string
	WaitTimeout time.Duration
}

func (o Options) Validate() error {
	if o.Platform != "appliance" {
		return ErrForbidden
	}
	if err := validatePath(o.Device); err != nil {
		return fmt.Errorf("state volume device: %w", err)
	}
	if !strings.HasPrefix(o.Device, "/dev/") {
		return errors.New("state volume device must be an explicit path below /dev")
	}
	if err := validatePath(o.Target); err != nil {
		return fmt.Errorf("state volume target: %w", err)
	}
	for _, reserved := range []string{"/proc", "/sys", "/dev"} {
		if within(o.Target, reserved) {
			return fmt.Errorf("state volume target must not cover a kernel interface: %s", reserved)
		}
	}
	if o.WaitTimeout < 0 || o.WaitTimeout > MaxWaitTimeout {
		return fmt.Errorf("state volume wait timeout must be between zero and %s", MaxWaitTimeout)
	}
	return nil
}

// Status describes the verified mount, or a partial result when a newly mounted
// volume fails a subsequent check. If Mounted is true and err is non-nil, the
// caller must stop startup; storage does not try to unmount a possibly replaced
// mountpoint. UUID is observed from the selected device, never used to find one.
type Status struct {
	Device         string   `json:"device"`
	Target         string   `json:"target"`
	FileSystem     string   `json:"filesystem"`
	DeviceMajor    uint32   `json:"device_major"`
	DeviceMinor    uint32   `json:"device_minor"`
	MountID        uint64   `json:"mount_id,omitempty"`
	UUID           string   `json:"uuid"`
	Mounted        bool     `json:"mounted"`
	AlreadyMounted bool     `json:"already_mounted"`
	MountOptions   []string `json:"mount_options,omitempty"`
	SuperOptions   []string `json:"super_options,omitempty"`
}

func validatePath(path string) error {
	if path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > maxPathBytes {
		return errors.New("path must be a clean, absolute, non-root path of at most 4095 bytes")
	}
	for _, c := range []byte(path) {
		if c < 0x20 || c == 0x7f {
			return errors.New("path must not contain control characters")
		}
	}
	return nil
}

func within(path, parent string) bool {
	return path == parent || parent == "/" && strings.HasPrefix(path, "/") || strings.HasPrefix(path, parent+"/")
}
