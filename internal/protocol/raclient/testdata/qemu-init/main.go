//go:build linux

// Test-only init, built solely by ../qemu.py. Production images never include it.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/kmod"
	"golang.org/x/sys/unix"
)

func run() error {
	if os.Getpid() != 1 {
		return errors.New("RA test init requires PID 1 in its dedicated virtual machine")
	}
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd"}); err != nil {
		return err
	}
	if err := kmod.Load([]string{"veth"}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/raclient.test", "-test.run=^TestIsolatedNativeRAClient$", "-test.v", "-test.timeout=45s")
	cmd.Env = []string{"PATH=/", "TMPDIR=/tmp", "ROUTERD2_INTEGRATION_REQUIRED=1"}
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	if !strings.Contains(output.String(), "--- PASS: TestIsolatedNativeRAClient") || strings.Contains(output.String(), "--- SKIP") {
		return errors.New("required native RA test did not execute and pass")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Println("ROUTERD_RA_CLIENT_TEST_FAIL:", err)
	} else {
		fmt.Println("ROUTERD_RA_CLIENT_TEST_PASS")
	}
	// Drain the virtual serial UART before firmware power-off closes it.
	_ = unix.IoctlSetInt(1, unix.TCSBRK, 1)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	for {
		time.Sleep(time.Second)
	}
}
