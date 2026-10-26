//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/clocktest"
	"golang.org/x/sys/unix"
)

type guestClockBroker struct {
	controller *clocktest.Controller
	conn       net.Conn
	client     *os.File
	done       chan error
}

func startClockBroker(s suite, command *exec.Cmd) (*guestClockBroker, error) {
	if s.test != "TestDaemonDHCPv4ClockCorrections" && s.test != "TestDaemonVRRPClockCorrections" && s.test != "TestDaemonDHCP6ClockCorrections" {
		return nil, nil
	}
	selected, err := os.ReadFile("/test-suite")
	if err != nil {
		return nil, err
	}
	nonce, err := os.ReadFile("/test-clock-nonce")
	if err != nil {
		return nil, err
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}
	controller, err := clocktest.NewController(clocktest.Guest{PID: os.Getpid(), Suite: strings.TrimSpace(string(selected)), Nonce: strings.TrimSpace(string(nonce)), CommandLine: string(cmdline)}, boottime.Read, func(target time.Time) error {
		// This syscall exists only in the dedicated test-image PID 1, after
		// validation of its suite, per-boot initramfs nonce and kernel marker.
		ts, err := unix.TimeToTimespec(target)
		if err != nil {
			return err
		}
		return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
	})
	if err != nil {
		return nil, err
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	serverFile := os.NewFile(uintptr(fds[0]), "clock-server")
	client := os.NewFile(uintptr(fds[1]), "clock-client")
	conn, err := net.FileConn(serverFile)
	_ = serverFile.Close()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	b := &guestClockBroker{controller: controller, conn: conn, client: client, done: make(chan error, 1)}
	command.ExtraFiles = []*os.File{client}
	command.Env = append(command.Env, clocktest.FDEnv+"=3")
	go func() { b.done <- clocktest.Serve(conn, controller) }()
	fmt.Println("ROUTERD_CLOCK_BROKER_READY fixed_offsets=+24h,-24h,restore scope=dedicated-guest-PID1")
	return b, nil
}

func (b *guestClockBroker) closeAndRestore() error {
	if b == nil {
		return nil
	}
	_ = b.client.Close()
	_ = b.conn.Close()
	serveErr := <-b.done
	if errors.Is(serveErr, net.ErrClosed) {
		serveErr = nil
	}
	_, restoreErr := b.controller.Step("restore")
	if restoreErr == nil {
		fmt.Println("ROUTERD_CLOCK_BROKER_RESTORED original_boot_baseline=true")
	}
	return errors.Join(serveErr, restoreErr)
}
