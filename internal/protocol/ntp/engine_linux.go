// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// Package ntp owns the fixed foreground chronyd engine and its private command
// socket. The host clock is adjusted only in explicit appliance deployments.
package ntp

import (
	"bytes"
	"context"
	"encoding/binary"
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

	"github.com/facebook/time/ntp/chrony"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/engineimage"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"golang.org/x/sys/unix"
)

type engineSettings struct {
	Config      ntpconfig.Config `json:"config"`
	AdjustClock bool             `json:"adjust_clock"`
}

func RunEngine() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	input := os.NewFile(3, "chronyd-config")
	var settings engineSettings
	err := json.NewDecoder(input).Decode(&settings)
	input.Close()
	if err != nil {
		return err
	}
	if err := ntpconfig.Validate(settings.Config); err != nil {
		return err
	}
	if err := unix.Unshare(unix.CLONE_NEWNS | unix.CLONE_FS); err != nil {
		return fmt.Errorf("create chronyd mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("isolate chronyd mounts: %w", err)
	}
	root := settings.Config.RuntimeDir
	if err := unix.Mount("tmpfs", root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
		return fmt.Errorf("mount chronyd temporary root: %w", err)
	}
	for _, directory := range []string{"/run/sources", "/state"} {
		if err := os.MkdirAll(root+directory, 0700); err != nil {
			return err
		}
	}
	// Resolve the generated state path in this mount namespace. An inherited
	// descriptor belongs to the parent's mount tree and cannot be bind-mounted.
	if err := unix.Mount(settings.Config.StateDir, root+"/state", "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind chronyd persistent state: %w", err)
	}
	if err := unix.Chroot(root); err != nil {
		return fmt.Errorf("enter chronyd root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	image := os.NewFile(4, "chronyd-image")
	if err := engineimage.Extract(image); err != nil {
		image.Close()
		return err
	}
	if err := image.Close(); err != nil {
		return err
	}
	if err := os.WriteFile("/chrony.conf", ntpconfig.Render(settings.Config.Node), 0600); err != nil {
		return err
	}
	var resolvers strings.Builder
	for _, address := range settings.Config.DNS {
		fmt.Fprintf(&resolvers, "nameserver %s\n", address)
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvers.String()), 0600); err != nil {
		return err
	}
	args := []string{"chronyd", "-d", "-u", "root", "-f", "/chrony.conf", "-F", "1"}
	if !settings.AdjustClock {
		args = append(args, "-x")
	}
	return syscall.Exec("/usr/sbin/chronyd", args, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"})
}

func Run(ctx context.Context, cfg ntpconfig.Config, image *os.File, updates <-chan ntpconfig.Sources, publish func(context.Context, ntpconfig.Snapshot) error, ready func()) error {
	if err := ntpconfig.Validate(cfg); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, path := range []string{cfg.StateDir, cfg.RuntimeDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	fd, err := unix.MemfdCreate("chronyd-config", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	input := os.NewFile(uintptr(fd), "chronyd-config")
	defer input.Close()
	adjust := os.Getenv("ROUTERD_PLATFORM") == "appliance"
	if err := json.NewEncoder(input).Encode(engineSettings{Config: cfg, AdjustClock: adjust}); err != nil {
		return err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(life, "/proc/self/exe", "chrony-engine")
	command.ExtraFiles = []*os.File{input, image}
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 2 * time.Second
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var exitErr error
	go func() { exitErr = command.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	base := "/proc/" + strconv.Itoa(command.Process.Pid) + "/root"
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(base + "/run/chronyd.sock"); err == nil {
			break
		}
		select {
		case <-done:
			return errors.Join(errors.New("chronyd startup exited"), exitErr)
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	root, err := os.Open(base)
	if err != nil {
		return err
	}
	defer root.Close()
	var sourceDone chan struct{}
	var sourceErr error
	if len(cfg.SourcesFrom) > 0 {
		sourceCtx, stop := context.WithCancel(ctx)
		sourceDone = make(chan struct{})
		defer func() { stop(); <-sourceDone }()
		go func() {
			sourceErr = runSources(sourceCtx, cfg, root, updates)
			close(sourceDone)
			if sourceErr != nil {
				cancel()
			}
		}()
	}
	ready()
	ticker.Reset(2 * time.Second)
	for {
		call, stop := context.WithTimeout(ctx, 2*time.Second)
		query := exec.CommandContext(call, "/proc/self/exe", "chrony-query")
		query.ExtraFiles = []*os.File{root}
		query.Stderr = os.Stderr
		query.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		query.WaitDelay = time.Second
		output, err := query.Output()
		stop()
		var snapshot ntpconfig.Snapshot
		if err == nil {
			err = json.Unmarshal(output, &snapshot)
		}
		if err != nil {
			snapshot.LastError = err.Error()
		}
		snapshot.AdjustingClock = adjust
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		snapshot.BootID = clock.BootID
		snapshot.ObservedNanos = uint64(clock.BoottimeNanoseconds)
		snapshot.ExpiresNanos = snapshot.ObservedNanos + uint64(5*time.Second)
		if err := publish(ctx, snapshot); err != nil && ctx.Err() == nil {
			slog.Warn("NTP observation reporting failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-sourceDone:
			return sourceErr
		case <-done:
			return errors.Join(errors.New("chronyd exited"), exitErr)
		case <-ticker.C:
		}
	}
}

type Command uint8

const (
	Tracking Command = iota
	ReloadSources
)

// RunCommand enters the running engine's pinned root, as the conntrack command
// boundary does. chronyd requires filesystem Unix addresses; abstract reply
// addresses are unsupported by its command protocol.
func RunCommand(command Command) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Fchdir(3); err != nil {
		return err
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Close(3); err != nil {
		return err
	}
	local := &net.UnixAddr{Name: "/run/query-" + strconv.Itoa(os.Getpid()) + ".sock", Net: "unixgram"}
	remote := &net.UnixAddr{Name: "/run/chronyd.sock", Net: "unixgram"}
	connection, err := net.DialUnix("unixgram", local, remote)
	if err != nil {
		return err
	}
	defer os.Remove(local.Name)
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if command == ReloadSources {
		// chrony 4.8 candm.h/pktlength.c: RELOAD_SOURCES has no body; the
		// 20-byte request header is padded to the 28-byte NULL reply length.
		request := struct {
			chrony.RequestHead
			Padding [8]byte
		}{RequestHead: chrony.RequestHead{Version: 6, PKTType: 1, Command: 70, Sequence: 1}}
		if err := binary.Write(connection, binary.BigEndian, request); err != nil {
			return err
		}
		var packet [1024]byte
		n, err := connection.Read(packet[:])
		if err != nil {
			return err
		}
		var reply chrony.ReplyHead
		if err := binary.Read(bytes.NewReader(packet[:n]), binary.BigEndian, &reply); err != nil {
			return err
		}
		if reply.Version != 6 || reply.PKTType != 2 || reply.Command != 70 || reply.Reply != 1 || reply.Sequence != 1 {
			return errors.New("chronyd returned an unexpected source-reload response")
		}
		if reply.Status != 0 {
			return fmt.Errorf("reload chronyd sources: %s", reply.Status)
		}
		return nil
	}
	if command != Tracking {
		return errors.New("unsupported chronyd command")
	}
	client := chrony.Client{Connection: connection}
	response, err := client.Communicate(chrony.NewTrackingPacket())
	if err != nil {
		return err
	}
	tracking, ok := response.(*chrony.ReplyTracking)
	if !ok || tracking.Sequence != client.Sequence {
		return errors.New("chronyd returned an unexpected tracking response")
	}
	source, _ := netip.AddrFromSlice(tracking.IPAddr)
	snapshot := ntpconfig.Snapshot{
		Synchronized: tracking.LeapStatus != 3 && tracking.Stratum > 0 && tracking.Stratum < 16,
		Source:       source.Unmap(), Stratum: tracking.Stratum, LeapStatus: tracking.LeapStatus,
		ReferenceTime: tracking.RefTime, OffsetSeconds: tracking.CurrentCorrection, RMSOffset: tracking.RMSOffset,
		FrequencyPPM: tracking.FreqPPM, RootDelay: tracking.RootDelay, RootDispersion: tracking.RootDispersion,
	}
	return json.NewEncoder(os.Stdout).Encode(snapshot)
}
