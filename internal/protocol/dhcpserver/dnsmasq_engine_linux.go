// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/imksoo/routerd2/internal/engineimage"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/sys/unix"
)

// RunEngine gives dnsmasq its own filesystem and the sole lease directory.
// dnsmasq owns packet processing and lease persistence; routerd owns its lifetime.
func RunEngine() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := os.NewFile(3, "dnsmasq-config")
	var settings engineSettings
	err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&settings)
	f.Close()
	if err != nil {
		return err
	}
	if err := unix.Unshare(unix.CLONE_NEWNS | unix.CLONE_FS); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	state, err := statefs.OpenDirectory(filepath.Dir(settings.Intent.StatePath), false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
		return err
	}
	root, err := os.MkdirTemp("/tmp", "dnsmasq-")
	if err != nil {
		return err
	}
	if err := os.Mkdir(root+"/state", 0700); err != nil {
		return err
	}
	if err := unix.Mount("/proc/thread-self/fd/"+strconv.Itoa(int(state.Fd())), root+"/state", "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := os.Mkdir(root+"/dev", 0700); err != nil {
		return err
	}
	dev, err := os.OpenFile(root+"/dev/urandom", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	dev.Close()
	if err := unix.Mount("/dev/urandom", root+"/dev/urandom", "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := unix.Chroot(root); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	image := os.NewFile(4, "dnsmasq-image")
	err = engineimage.Extract(image)
	image.Close()
	if err != nil {
		return err
	}
	if err := os.WriteFile("/etc/dnsmasq.conf", dnsmasqConfig(settings), 0600); err != nil {
		return err
	}
	return syscall.Exec("/usr/sbin/dnsmasq", []string{"dnsmasq", "--keep-in-foreground", "--conf-file=/etc/dnsmasq.conf"}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "ROUTERD_DHCP_EVENT_SOCKET=" + settings.EventSocket})
}
