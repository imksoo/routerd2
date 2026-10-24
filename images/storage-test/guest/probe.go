// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type executionWitness struct {
	Phase              string   `json:"phase"`
	BootID             string   `json:"boot_id"`
	PID                int      `json:"pid"`
	ReleaseID          string   `json:"release_id"`
	ReleaseOperation   string   `json:"release_operation"`
	ReleaseGeneration  uint64   `json:"release_generation"`
	Pending            bool     `json:"pending"`
	FailedBootID       string   `json:"failed_boot_id,omitempty"`
	Revision           uint64   `json:"revision"`
	ConfigSHA256       string   `json:"config_sha256"`
	HostKeySHA256      string   `json:"host_key_sha256"`
	HostKeyFingerprint string   `json:"host_key_fingerprint"`
	SentinelSHA256     string   `json:"sentinel_sha256"`
	Modules            []string `json:"modules"`
	PinnedExecVerified bool     `json:"pinned_exec_verified"`
	Filesystem         string   `json:"filesystem"`
}

func runProbe(phase string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	processes, health, err := awaitReady(ctx, phase)
	if err != nil {
		return err
	}
	if err := requireNoNIC(); err != nil {
		return err
	}
	if err := verifyMountedData(); err != nil {
		return err
	}
	expected, err := readWitness()
	if err != nil {
		return err
	}
	bootID, err := kernelBootID()
	if err != nil {
		return err
	}
	selected := expected.Trial
	if phase == "second" {
		selected = expected.Baseline
		if bootID == expected.FirstBootID {
			return errors.New("second phase is not a new kernel boot")
		}
	} else if bootID != expected.FirstBootID {
		return errors.New("boot ID changed during first PID 1 handoff")
	}
	names, err := verifyExecutables(processes, selected)
	if err != nil {
		return err
	}
	hostPath := filepath.Join(stateDir, "ssh_host_ed25519_key")
	hostBytes, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	host, err := ssh.ParsePrivateKey(hostBytes)
	clear(hostBytes)
	if err != nil {
		return errors.New("product host key cannot be parsed")
	}
	fingerprint := ssh.FingerprintSHA256(host.PublicKey())
	if phase == "second" && fingerprint != expected.HostKeyFingerprint {
		return errors.New("SSH host public key changed across disk remount")
	}
	client, err := connectSSH(ctx, host.PublicKey())
	if err != nil {
		return err
	}
	defer client.Close()
	if phase == "first" {
		if health.Revision != 1 {
			return errors.New("first boot did not start from the seeded configuration")
		}
		if err := commitThroughSSH(ctx, client); err != nil {
			return err
		}
		health, err = readHealthyConfig(ctx)
		if err != nil {
			return err
		}
	} else {
		// This is a fresh authenticated SSH connection to the persistent host
		// key after kernel reboot; inspect committed state through that channel.
		session, err := client.NewSession()
		if err != nil {
			return err
		}
		output, err := session.Output("show config")
		session.Close()
		if err != nil {
			return fmt.Errorf("SSH config read after reboot: %w", err)
		}
		var state control.State
		if err := json.Unmarshal(output, &state); err != nil || state.Revision != expected.Revision {
			return errors.New("SSH did not return the persisted committed revision")
		}
	}
	plan, err := config.Compile(health.Config)
	if err != nil {
		return err
	}
	if health.Revision != 2 || health.Confirmation != nil || len(plan.Native.Network.Interfaces) != 1 || plan.Native.Network.Interfaces[0].Name != "lo" || plan.Native.Network.Interfaces[0].Alias == nil || *plan.Native.Network.Interfaces[0].Alias != persistentAlias {
		return errors.New("committed SSH alias edit did not persist in the control state")
	}
	loopback, err := netlink.LinkByName("lo")
	if err != nil || loopback.Attrs().Alias != persistentAlias {
		return errors.New("persisted configuration was not projected onto the new kernel")
	}
	committed, err := (&config.Store{Dir: stateDir}).Load()
	if err != nil || committed.Revision != 2 || committed.Confirmation != nil {
		return errors.New("persistent committed configuration record is missing")
	}
	configHash, err := fileHash(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return err
	}
	hostHash, err := fileHash(hostPath)
	if err != nil {
		return err
	}
	sentinelHash, err := fileHash(sentinelPath)
	if err != nil {
		return err
	}
	if phase == "second" && (configHash != expected.ConfigSHA256 || hostHash != expected.HostKeySHA256 || sentinelHash != expected.SentinelSHA256) {
		return errors.New("persistent config, host key or state sentinel changed between kernels")
	}
	keys, err := release.LoadTrustFile(trustPath)
	if err != nil {
		return err
	}
	store, err := release.Open(release.Options{Root: storeRoot, TrustedKeys: keys, TargetOS: "linux", TargetArch: "amd64"})
	if err != nil {
		return err
	}
	defer store.Close()
	state, err := store.Status(ctx)
	if err != nil {
		return err
	}
	if state.Active == nil || *state.Active != selected || state.HighWatermark != expected.Trial.Sequence {
		return errors.New("running signed release does not match persistent journal")
	}
	if phase == "first" {
		if state.Operation != "begin-boot" || state.Pending == nil || state.Pending.Release != expected.Trial || state.Pending.MaxAttempts != 1 || state.Attempt == nil || state.Attempt.BootID != bootID || state.Attempt.Number != 1 || state.Previous == nil || *state.Previous != expected.Baseline {
			return errors.New("first boot no longer has its bounded unconfirmed trial")
		}
		expected.Revision, expected.ConfigSHA256 = health.Revision, configHash
		expected.HostKeySHA256, expected.HostKeyFingerprint = hostHash, fingerprint
		expected.SentinelSHA256, expected.ReleaseGeneration = sentinelHash, state.Generation
		if err := config.AtomicJSON(witnessPath, expected); err != nil {
			return err
		}
	} else if state.Operation != "boot-rollback" || state.Pending != nil || state.Attempt != nil || state.Previous != nil || state.LastFailure == nil || state.LastFailure.Release != expected.Trial || state.LastFailure.BootID != expected.FirstBootID || state.Generation != expected.ReleaseGeneration+1 {
		return errors.New("fresh kernel did not automatically fall back from its spent trial")
	}
	witness := executionWitness{
		Phase: phase, BootID: bootID, PID: processes.PID, ReleaseID: selected.ReleaseID,
		ReleaseOperation: state.Operation, ReleaseGeneration: state.Generation,
		Pending: state.Pending != nil, Revision: health.Revision,
		ConfigSHA256: configHash, HostKeySHA256: hostHash, HostKeyFingerprint: fingerprint,
		SentinelSHA256: sentinelHash, Modules: names, PinnedExecVerified: true, Filesystem: "ext4",
	}
	if state.LastFailure != nil {
		witness.FailedBootID = state.LastFailure.BootID
	}
	data, err := json.Marshal(witness)
	if err != nil {
		return err
	}
	fmt.Printf("%s_WITNESS %s\n", marker, data)
	return nil
}

func awaitReady(ctx context.Context, phase string) (supervisor.Status, control.State, error) {
	want := trialID
	if phase == "second" {
		want = baselineID
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			return supervisor.Status{}, control.State{}, fmt.Errorf("actual PID 1 did not become ready: %w", last)
		case <-timer.C:
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			var processes supervisor.Status
			err := localapi.NewClient(filepath.Join(runtimeDir, supervisor.SocketName)).Do(call, "GET", "/v1/status", nil, &processes)
			var health control.State
			if err == nil {
				if processes.PID != 1 || processes.Platform != "appliance" || !processes.Ready || processes.Stopping || processes.ReleaseID != want {
					err = errors.New("actual supervisor has not selected and started the expected release")
				} else {
					health, err = readHealthyConfig(call)
				}
			}
			cancel()
			if err == nil {
				return processes, health, nil
			}
			last = err
			timer.Reset(500 * time.Millisecond)
		}
	}
}

func readHealthyConfig(ctx context.Context) (control.State, error) {
	var health control.State
	err := localapi.NewClient(filepath.Join(runtimeDir, control.SocketName)).Do(ctx, "GET", "/v1/config", nil, &health)
	if err == nil && (!health.Healthy || health.Revision == 0 || health.Services == nil || health.Services.Phase != "ready" || health.Services.Revision != health.Revision || health.Services.RouteIssueCount != 0) {
		err = errors.New("committed routing configuration is not healthy")
	}
	return health, err
}

func verifyExecutables(processes supervisor.Status, selected release.Ref) ([]string, error) {
	if len(processes.Modules) != 4 {
		return nil, errors.New("unexpected supervised core process count")
	}
	if err := sameExecutable(1, filepath.Join(storeRoot, "releases", selected.Digest, "routerd")); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, module := range processes.Modules {
		if !module.Ready || module.PID <= 1 || module.ReleaseID != selected.ReleaseID || seen[module.Name] {
			return nil, errors.New("selected command pair did not start each core module")
		}
		if module.Name != "logging" && !module.Required {
			return nil, errors.New("required core module became optional")
		}
		if err := sameExecutable(module.PID, filepath.Join(storeRoot, "releases", selected.Digest, "routerd-modules")); err != nil {
			return nil, err
		}
		seen[module.Name] = true
		names = append(names, module.Name)
	}
	for _, name := range []string{"logging", "network", "control", "management"} {
		if !seen[name] {
			return nil, fmt.Errorf("missing core module %s", name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func sameExecutable(pid int, path string) error {
	current, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	expected, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(current, expected) {
		return fmt.Errorf("PID %d does not execute its verified persistent release inode", pid)
	}
	return nil
}

func verifyMountedData() error {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(stateDir, &filesystem); err != nil {
		return err
	}
	if filesystem.Type != unix.EXT4_SUPER_MAGIC {
		return errors.New("product state directory is not backed by ext4")
	}
	var node, directory unix.Stat_t
	if err := unix.Stat(devicePath, &node); err != nil {
		return err
	}
	if err := unix.Stat(stateDir, &directory); err != nil {
		return err
	}
	if node.Mode&unix.S_IFMT != unix.S_IFBLK || node.Rdev != directory.Dev {
		return errors.New("product mounted a different backing device")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[4] != stateDir {
			continue
		}
		options := map[string]bool{}
		for _, value := range strings.Split(fields[5], ",") {
			options[value] = true
		}
		if fields[3] != "/" || !options["rw"] || !options["nosuid"] || !options["nodev"] || options["noexec"] {
			return errors.New("persistent mount lacks its required safe executable filesystem flags")
		}
		return nil
	}
	return errors.New("product state directory is not its own mounted filesystem")
}

func connectSSH(ctx context.Context, host ssh.PublicKey) (*ssh.Client, error) {
	client, err := clientSigner()
	if err != nil {
		return nil, err
	}
	// The local guest listener is already ready; this authenticates both the
	// client and the exact host key that must remain stable across kernels.
	return ssh.Dial("tcp", sshAddress, &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.PublicKeys(client)}, HostKeyCallback: ssh.FixedHostKey(host), Timeout: 5 * time.Second})
}

func commitThroughSSH(ctx context.Context, client *ssh.Client) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	// SSH copies stdout and stderr concurrently. Separate buffers are only
	// inspected after Wait has joined both copy goroutines.
	var transcript, diagnostics bytes.Buffer
	session.Stdout, session.Stderr = &transcript, &diagnostics
	input, err := session.StdinPipe()
	if err != nil {
		return err
	}
	if err := session.RequestPty("xterm", 24, 100, ssh.TerminalModes{}); err != nil {
		return err
	}
	if err := session.Shell(); err != nil {
		return err
	}
	if _, err := io.WriteString(input, "configure\rset resources[interface/lo].interface.alias \""+persistentAlias+"\"\rvalidate\rcommit\rexit\rexit\r"); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("SSH candidate commit: %w; %s; %s", err, transcript.String(), diagnostics.String())
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("SSH candidate commit exceeded its fixture deadline")
	}
}
