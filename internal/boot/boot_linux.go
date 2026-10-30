//go:build linux

// Package boot contains the minimal appliance bootstrap. It does not discover,
// format, or mount a persistent data disk; StateDir must be backed by the storage
// selected by the image's boot configuration.
package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	Appliance = "appliance"
	Container = "container"
	Host      = "host"
)

type Options struct {
	Platform   string
	RuntimeDir string
	StateDir   string
}

type mountSpec struct {
	source, target, fs, data string
	flags                    uintptr
}

// Bootstrap mounts kernel filesystems and enables loopback only when appliance
// mode is explicitly selected and the caller is PID 1. Container PID 1 is never
// inferred to mean appliance mode. Repeated calls retain the prepared state.
func Bootstrap(o Options) error {
	if err := bootstrap(o, os.Getpid(), mountBasic, makeDirectories); err != nil {
		return err
	}
	if o.Platform == Appliance {
		if err := enableLoopback(); err != nil {
			return fmt.Errorf("enable appliance loopback: %w", err)
		}
	}
	return nil
}

// PID 1 supplies the local transport used by services such as chained DNS.
// Host and container networking remain with their enclosing system.
func enableLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	request, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, request); err != nil {
		return err
	}
	request.SetUint16(request.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, request)
}

func bootstrap(o Options, pid int, mount func() error, directories func(Options) error) error {
	switch o.Platform {
	case Appliance:
		if pid != 1 {
			return fmt.Errorf("appliance bootstrap requires PID 1 (got %d)", pid)
		}
	case Container, Host:
	default:
		return fmt.Errorf("platform must be explicitly appliance, container, or host: %q", o.Platform)
	}
	if o.Platform == Appliance {
		if err := mount(); err != nil {
			return err
		}
	}
	return directories(o)
}

func mountBasic() error {
	noDevices := uintptr(unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV)
	mounts := []mountSpec{
		{"proc", "/proc", "proc", "", noDevices},
		{"sysfs", "/sys", "sysfs", "", noDevices},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID},
		{"devpts", "/dev/pts", "devpts", "newinstance,ptmxmode=0666,mode=0620", unix.MS_NOSUID | unix.MS_NOEXEC},
		{"tmpfs", "/run", "tmpfs", "mode=0755", unix.MS_NOSUID | unix.MS_NODEV},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0755); err != nil {
			return fmt.Errorf("create mountpoint %s: %w", m.target, err)
		}
		mountedFS, mounted, err := mountedAt(m.target)
		if err != nil {
			return err
		}
		if mounted {
			if mountedFS != m.fs {
				return fmt.Errorf("mountpoint %s already contains %s, expected %s", m.target, mountedFS, m.fs)
			}
			continue
		}
		if err := unix.Mount(m.source, m.target, m.fs, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fs, m.target, err)
		}
	}
	if _, err := os.Lstat("/dev/ptmx"); os.IsNotExist(err) {
		if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil {
			return fmt.Errorf("create /dev/ptmx: %w", err)
		}
	}
	return nil
}

func mountedAt(target string) (string, bool, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if os.IsNotExist(err) {
		return "", false, nil // /proc has not been mounted yet.
	}
	if err != nil {
		return "", false, fmt.Errorf("read mount table: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[4] != target {
			continue
		}
		for i := 6; i+1 < len(fields); i++ {
			if fields[i] == "-" {
				return fields[i+1], true, nil
			}
		}
	}
	return "", false, nil
}

func makeDirectories(o Options) error {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{o.RuntimeDir, 0750}, {o.StateDir, 0700}} {
		if d.path == "" {
			continue
		}
		if !filepath.IsAbs(d.path) || filepath.Clean(d.path) == "/" {
			return fmt.Errorf("routerd directory must be an absolute non-root path: %q", d.path)
		}
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("create %s: %w", d.path, err)
		}
		info, err := os.Lstat(d.path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("routerd directory is not a real directory: %s", d.path)
		}
	}
	return nil
}
