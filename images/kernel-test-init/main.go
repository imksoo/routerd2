//go:build linux

// The kernel integration image has its own test-only PID 1. It is never copied
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

type suite struct {
	binary, test, marker string
	timeout              time.Duration
}

func selectedSuite() (suite, error) {
	data, err := os.ReadFile("/test-suite")
	if err != nil {
		return suite{}, fmt.Errorf("read test suite: %w", err)
	}
	switch strings.TrimSpace(string(data)) {
	case "bfd":
		return suite{"/bfd.test", "TestIsolatedNativeBFD", "ROUTERD_BFD_NATIVE_TEST", 60 * time.Second}, nil
	case "wireguard":
		return suite{"/network.test", "TestIsolatedKernelWireGuardOwnershipAndTraffic", "ROUTERD_WIREGUARD_TEST", 75 * time.Second}, nil
	case "network":
		return suite{"/network.test", "TestIsolatedKernelForwardingNATAndDSLite", "ROUTERD_NETWORK_TEST", 55 * time.Second}, nil
	case "ra-dns":
		return suite{"/ra.test", "TestIsolatedLearnedDNSLifetimes", "ROUTERD_RA_DNS_TEST", 45 * time.Second}, nil
	case "daemon":
		return suite{"/daemon.test", "TestDaemonRAClientConfigurationAndExpiry", "ROUTERD_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-aftr":
		return suite{"/daemon.test", "TestDaemonAFTRResolutionAndExpiry", "ROUTERD_AFTR_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-management":
		return suite{"/daemon.test", "TestDaemonConfigurationAndRecovery", "ROUTERD_MANAGEMENT_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-eventrule":
		return suite{"/daemon.test", "TestDaemonEventRuleDurabilityAndLifecycle", "ROUTERD_EVENTRULE_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-derived":
		return suite{"/daemon.test", "TestDaemonDerivedHealthCurrentStatusAndDurability", "ROUTERD_DERIVED_DAEMON_TEST", 170 * time.Second}, nil
	case "daemon-stopped-health":
		return suite{"/daemon.test", "TestDaemonStoppedHealthHistoryWithoutReactivation", "ROUTERD_STOPPED_HEALTH_DAEMON_TEST", 110 * time.Second}, nil
	case "daemon-resource-program":
		return suite{"/daemon.test", "TestDaemonResourceProgramLifecycleAndRecovery", "ROUTERD_RESOURCE_PROGRAM_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-health-pbr":
		return suite{"/daemon.test", "TestDaemonHealthPBRFailoverAndRecovery", "ROUTERD_HEALTH_PBR_DAEMON_TEST", 210 * time.Second}, nil
	case "daemon-dhcp-dns":
		return suite{"/daemon.test", "TestDaemonDHCPDNSAndOwnershipExpiry", "ROUTERD_DHCP_DNS_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-legacy-dhcp-dns":
		return suite{"/daemon.test", "TestDaemonMigratedDHCPDNSAndOwnershipExpiry", "ROUTERD_LEGACY_DHCP_DNS_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-bgp":
		return suite{"/daemon.test", "TestDaemonBGPFIBAndOwnershipExpiry", "ROUTERD_BGP_DAEMON_TEST", 150 * time.Second}, nil
	case "daemon-bfd":
		return suite{"/daemon.test", "TestDaemonBFDControlsBGPSessionAndExpiry", "ROUTERD_BFD_DAEMON_TEST", 150 * time.Second}, nil
	case "dhcp":
		return suite{"/dhcp.test", "TestIsolatedNativeDHCPAndRA", "ROUTERD_DHCP_NATIVE_TEST", 75 * time.Second}, nil
	case "dhcp-server":
		return suite{"/dhcp-server.test", "TestIsolatedNativeDHCPServer", "ROUTERD_DHCP_SERVER_TEST", 100 * time.Second}, nil
	case "vrrp":
		return suite{"/vrrp.test", "TestIsolatedNativeVRRP", "ROUTERD_VRRP_NATIVE_TEST", 75 * time.Second}, nil
	case "ra-client":
		return suite{"/ra-client.test", "TestIsolatedNativeRAClient", "ROUTERD_RA_CLIENT_TEST", 60 * time.Second}, nil
	case "aftr":
		return suite{"/aftr.test", "TestIsolatedNativeAFTR", "ROUTERD_AFTR_NATIVE_TEST", 50 * time.Second}, nil
	default:
		return suite{}, fmt.Errorf("unrecognized fixed test suite")
	}
}

func run(s suite) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("test init requires PID 1 in its dedicated virtual machine")
	}
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd"}); err != nil {
		return err
	}
	// The immutable image already occupies most of the small default rootfs
	// quota. SQLite needs writable space for both source and target WALs even
	// when their retained payload is small. Provision test scratch explicitly;
	// production boot/storage and the VM's RAM size are not changed here.
	if err := os.MkdirAll("/tmp", 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir("/tmp")
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("test scratch mountpoint is not empty")
	}
	if err = unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=64m"); err != nil {
		return fmt.Errorf("mount test scratch: %w", err)
	}
	var scratch unix.Statfs_t
	if err = unix.Statfs("/tmp", &scratch); err != nil {
		return err
	}
	fmt.Printf("ROUTERD_FIXTURE_SCRATCH total_bytes=%d available_bytes=%d\n", scratch.Blocks*uint64(scratch.Bsize), scratch.Bavail*uint64(scratch.Bsize))
	if err := kmod.Load([]string{"af_packet", "veth", "bridge", "8021q", "vrf", "vxlan", "macvlan", "wireguard", "nf_tables", "nft_ct", "nft_chain_nat", "nft_masq", "nft_nat", "nft_reject_inet", "nft_log", "nf_log_syslog", "ip6_tunnel"}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout+5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, s.binary, "-test.run=^"+s.test+"$", "-test.v", "-test.timeout="+s.timeout.String())
	command.Env = []string{"PATH=/", "TMPDIR=/tmp", "ROUTERD2_INTEGRATION_REQUIRED=1"}
	if s.binary == "/daemon.test" {
		command.Env = append(command.Env, "ROUTERD2_TEST_BIN_DIR=/test/bin")
	}
	var output bytes.Buffer
	writer := io.MultiWriter(os.Stdout, &output)
	command.Stdout, command.Stderr = writer, writer
	if err := command.Run(); err != nil {
		return err
	}
	// A renamed test or a mismatched -test.run pattern must not produce a
	// false pass merely because the Go test executable exits successfully.
	if !strings.Contains(output.String(), "--- PASS: "+s.test+" (") || strings.Contains(output.String(), "--- SKIP:") {
		return fmt.Errorf("required integration test did not report PASS")
	}
	return nil
}

func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "kernel fixture initializer requires PID 1; refusing bootstrap or poweroff")
		os.Exit(1)
	}
	s, err := selectedSuite()
	if err == nil {
		err = run(s)
	}
	if s.marker == "" {
		s.marker = "ROUTERD_KERNEL_TEST"
	}
	if err == nil {
		fmt.Println(s.marker + "_PASS")
	} else {
		fmt.Println(s.marker+"_FAIL:", err)
	}
	// Filesystem Sync does not drain a UART. Wait for the terminal's queued
	// test output before cutting power, otherwise a fast guest can shut down
	// while the host serial log still contains only early boot messages.
	if err := unix.IoctlSetInt(int(os.Stdout.Fd()), unix.TCSBRK, 1); err != nil {
		fmt.Fprintln(os.Stderr, "test console drain failed:", err)
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
