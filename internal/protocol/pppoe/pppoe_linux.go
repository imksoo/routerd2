//go:build linux

// Package pppoe owns one foreground pppd session. pppd owns the PPP unit and
// negotiated addresses; the routerd network writer owns forwarding and routes.
package pppoe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/pppimage"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// RunEngine is the short-lived exec boundary used by the PPP worker. Its new
// mount namespace confines pppd's fixed /etc/ppp hooks, DNS file and /run PID
// files. No shell hooks or operator-selected executable run in the host mount.
func RunEngine() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := os.NewFile(3, "pppoe-config")
	var cfg pppoeconfig.Config
	err := json.NewDecoder(io.LimitReader(f, 65536)).Decode(&cfg)
	_ = f.Close()
	if err != nil {
		return err
	}
	secretFile, err := os.Open(cfg.PasswordFile)
	if err != nil {
		return fmt.Errorf("read PPPoE password: %w", err)
	}
	password, err := io.ReadAll(io.LimitReader(secretFile, 4098))
	_ = secretFile.Close()
	if err != nil {
		return fmt.Errorf("read PPPoE password: %w", err)
	}
	secret := strings.TrimSuffix(string(password), "\n")
	if len(secret) > 4096 || strings.ContainsAny(secret, "\x00\r\n") {
		return errors.New("PPPoE password must be one line of at most 4096 bytes")
	}
	if err := unix.Unshare(unix.CLONE_NEWNS | unix.CLONE_FS); err != nil {
		return fmt.Errorf("create pppd mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("isolate pppd mount propagation: %w", err)
	}
	// All executable/library/configuration lookup stays in the pinned image.
	// Its empty root lives only in this mount namespace, so even a killed
	// helper leaves no extracted files or mountpoint directories in the host.
	if err := os.MkdirAll("/tmp", 0700); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
		return err
	}
	root, err := os.MkdirTemp("/tmp", "ppp-")
	if err != nil {
		return err
	}
	if err := unix.Chroot(root); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	image := os.NewFile(4, "pppoe-image")
	if err := pppimage.Extract(image); err != nil {
		image.Close()
		return err
	}
	if err := image.Close(); err != nil {
		return err
	}
	for _, path := range []string{"/etc/ppp", "/run", "/dev"} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		flags := unix.MS_NOSUID | unix.MS_NOEXEC
		if path != "/dev" {
			flags |= unix.MS_NODEV
		}
		if err := unix.Mount("tmpfs", path, "tmpfs", uintptr(flags), "mode=700"); err != nil {
			return fmt.Errorf("isolate pppd %s: %w", path, err)
		}
	}
	for _, device := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"ppp", 108, 0}, {"urandom", 1, 9}} {
		if err := unix.Mknod("/dev/"+device.name, unix.S_IFCHR|0600, int(unix.Mkdev(device.major, device.minor))); err != nil {
			return err
		}
	}
	if err := os.Mkdir("/run/ppp", 0700); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/ppp/options", peerOptions(cfg, secret), 0600); err != nil {
		return err
	}
	// The installed pppd may be setuid. NO_NEW_PRIVS preserves PDEATHSIG over
	// that exec; the worker already has the privileges needed for PPP.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	return syscall.Exec("/"+pppimage.Command, []string{"pppd", "logfd", "1"}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"})
}

// Adapted from routerd pkg/pppoeclient.LinuxPeer (BSD-3-Clause, 1329ef54f296).
// Use the canonical plugin filename: Alpine pppd 2.5 has no rp-pppoe.so alias.
func peerOptions(cfg pppoeconfig.Config, password string) []byte {
	if cfg.MTU == 0 {
		cfg.MTU = 1454
	}
	if cfg.MRU == 0 {
		cfg.MRU = 1454
	}
	if cfg.LCPEchoInterval == 0 {
		cfg.LCPEchoInterval = 30
	}
	if cfg.LCPEchoFailure == 0 {
		cfg.LCPEchoFailure = 4
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "plugin /%s\nnic-%s\nifname %s\nuser %s\npassword %s\n", pppimage.Plugin, cfg.Interface, cfg.PPPInterface, strconv.Quote(cfg.Username), strconv.Quote(password))
	b.WriteString("noauth\nhide-password\nnoipdefault\nusepeerdns\nnodetach\n")
	switch cfg.AuthMethod {
	case "pap":
		b.WriteString("-chap\n")
	case "", "chap":
		b.WriteString("-pap\n")
	}
	if cfg.ServiceName != "" {
		fmt.Fprintf(&b, "rp_pppoe_service %s\n", strconv.Quote(cfg.ServiceName))
	}
	if cfg.ACName != "" {
		fmt.Fprintf(&b, "rp_pppoe_ac %s\n", strconv.Quote(cfg.ACName))
	}
	fmt.Fprintf(&b, "mtu %d\nmru %d\nlcp-echo-interval %d\nlcp-echo-failure %d\n", cfg.MTU, cfg.MRU, cfg.LCPEchoInterval, cfg.LCPEchoFailure)
	if cfg.IPv6 {
		b.WriteString("+ipv6\n")
	} else {
		b.WriteString("-ipv6\n")
	}
	return b.Bytes()
}

func Run(ctx context.Context, cfg pppoeconfig.Config, image *os.File, publish func(context.Context, pppoeconfig.Snapshot) error, ready func()) error {
	if err := pppoeconfig.Validate(cfg); err != nil {
		return err
	}
	if _, err := net.InterfaceByName(cfg.PPPInterface); err == nil {
		return fmt.Errorf("PPP interface %s already exists; refusing a second owner", cfg.PPPInterface)
	}
	fd, err := unix.MemfdCreate("pppoe-config", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "pppoe-config")
	defer f.Close()
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// Linux ties PDEATHSIG to the creating thread, which must outlive pppd.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command("/proc/self/exe", "pppoe-engine")
	cmd.ExtraFiles = []*os.File{f, image}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if exited {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ready()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		snapshot, err := observe(cfg, cmd.Process.Pid)
		if err != nil {
			return err
		}
		if err := publish(ctx, snapshot); err != nil && ctx.Err() == nil {
			slog.Warn("publish PPPoE observation", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			exited = true
			if err == nil {
				return errors.New("pppd session ended")
			}
			return fmt.Errorf("pppd session ended: %w", err)
		case <-tick.C:
		}
	}
}

func observe(cfg pppoeconfig.Config, pid int) (pppoeconfig.Snapshot, error) {
	clock, err := boottime.Read()
	if err != nil {
		return pppoeconfig.Snapshot{}, err
	}
	s := pppoeconfig.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, PPPInterface: cfg.PPPInterface, ObservedNanos: uint64(clock.BoottimeNanoseconds), ValidUntil: lifetime.After(clock, 5)}
	link, err := netlink.LinkByName(cfg.PPPInterface)
	if err != nil {
		var absent netlink.LinkNotFoundError
		if errors.As(err, &absent) {
			return s, nil
		}
		return s, err
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return s, err
	}
	for _, a := range addresses {
		if cfg.IPv6 && a.IP.IsLinkLocalUnicast() && a.IP.To4() == nil && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			s.Active, s.IPv6Address = true, a.IP.String()
		}
	}
	for _, a := range addresses {
		if a.Peer == nil || a.IP.To4() == nil || a.Peer.IP.To4() == nil {
			continue
		}
		s.Active, s.Address, s.Peer = true, a.IP.String(), a.Peer.IP.String()
		// pppd's usepeerdns file is private to this exact child mount namespace.
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/root/run/ppp/resolv.conf", pid))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read negotiated PPP DNS", "error", err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "nameserver" {
				if dns, err := netip.ParseAddr(fields[1]); err == nil && dns.Is4() && !dns.IsUnspecified() && !dns.IsMulticast() {
					s.DNSServers = append(s.DNSServers, dns.String())
				}
			}
		}
		break
	}
	return s, nil
}
