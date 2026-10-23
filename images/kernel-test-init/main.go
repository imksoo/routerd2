//go:build linux

// The network integration image has its own test-only PID 1. It is never copied
// into the production appliance: no production management command can execute
// the test binary or an arbitrary program.
package main

import (
	"bytes"
	"context"
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
		return fmt.Errorf("test init requires PID 1 in its dedicated virtual machine")
	}
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd"}); err != nil {
		return err
	}
	if err := kmod.Load([]string{"veth", "bridge", "8021q", "vrf", "vxlan", "macvlan", "nf_tables", "nft_ct", "nft_chain_nat", "nft_masq", "nft_nat", "nft_reject_inet", "nft_log", "nf_log_syslog", "ip6_tunnel"}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/network.test", "-test.run=^TestIsolatedKernelForwardingNATAndDSLite$", "-test.v", "-test.timeout=55s")
	command.Env = []string{"PATH=/", "TMPDIR=/tmp", "ROUTERD2_INTEGRATION_REQUIRED=1"}
	var output bytes.Buffer
	writer := io.MultiWriter(os.Stdout, &output)
	command.Stdout, command.Stderr = writer, writer
	if err := command.Run(); err != nil {
		return err
	}
	// A renamed test or a mismatched -test.run pattern must not produce a
	// false pass merely because the Go test executable exits successfully.
	if !strings.Contains(output.String(), "--- PASS: TestIsolatedKernelForwardingNATAndDSLite") {
		return fmt.Errorf("required integration test did not report PASS")
	}
	return nil
}

func main() {
	err := run()
	if err == nil {
		fmt.Println("ROUTERD_NETWORK_TEST_PASS")
	} else {
		fmt.Println("ROUTERD_NETWORK_TEST_FAIL:", err)
	}
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Fprintln(os.Stderr, "test guest poweroff failed:", err)
	}
	// PID 1 must never return, even when the poweroff syscall is unavailable.
	for {
		time.Sleep(time.Second)
	}
}
