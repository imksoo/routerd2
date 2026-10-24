// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// This initializer/probe belongs only to the no-NIC, private-disk integration
// guest. It is never shipped in the production appliance or management shell.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/storage"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const (
	marker          = "ROUTERD_STORAGE_TEST"
	guestFlag       = "routerd2_storage_fixture=1"
	baselineID      = "storage-gate-baseline"
	trialID         = "storage-gate-trial"
	devicePath      = "/dev/vda"
	stateDir        = "/var/lib/routerd"
	runtimeDir      = "/run/routerd"
	storeRoot       = stateDir + "/releases"
	trustPath       = stateDir + "/release-keys.json"
	clientPath      = stateDir + "/fixture-client-key"
	seedPath        = stateDir + "/bootstrap.json"
	witnessPath     = stateDir + "/fixture-witness.json"
	sentinelPath    = stateDir + "/fixture-sentinel"
	bootPath        = "/etc/routerd/boot.json"
	sshAddress      = "127.0.0.1:2222"
	persistentAlias = "persistent-two-boot"
)

type diskWitness struct {
	Baseline           release.Ref `json:"baseline"`
	Trial              release.Ref `json:"trial"`
	FirstBootID        string      `json:"first_boot_id"`
	Revision           uint64      `json:"revision"`
	ConfigSHA256       string      `json:"config_sha256"`
	HostKeySHA256      string      `json:"host_key_sha256"`
	HostKeyFingerprint string      `json:"host_key_fingerprint"`
	SentinelSHA256     string      `json:"sentinel_sha256"`
	ReleaseGeneration  uint64      `json:"release_generation"`
}

func main() {
	probe := len(os.Args) == 2 && os.Args[1] == "probe"
	if (!probe && (len(os.Args) != 1 || os.Getpid() != 1)) || (probe && os.Getpid() <= 1) {
		abortBeforeGuest(errors.New("fixture requires its dedicated guest init or independent probe"))
	}
	if !probe {
		if _, err := os.Stat("/proc/cmdline"); errors.Is(err, os.ErrNotExist) {
			if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
				abortBeforeGuest(err)
			}
		}
	}
	phase, err := guestPhase()
	if err != nil {
		abortBeforeGuest(err)
	}
	if probe {
		err = runProbe(phase)
	} else {
		err = initializeAndExec(phase)
	}
	if err != nil {
		fmt.Println(marker+"_FAIL:", err)
	} else {
		fmt.Println(marker + "_" + strings.ToUpper(phase) + "_PASS")
	}
	poweroff()
}

func abortBeforeGuest(err error) {
	fmt.Fprintln(os.Stderr, marker+"_FAIL:", err)
	if os.Getpid() != 1 {
		os.Exit(1)
	}
	for {
		time.Sleep(time.Second)
	}
}

func guestPhase() (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("fixture requires guest root")
	}
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", err
	}
	dedicated, phase := false, ""
	for _, field := range strings.Fields(string(data)) {
		if field == guestFlag {
			dedicated = true
		}
		if strings.HasPrefix(field, "routerd2_storage_phase=") {
			if phase != "" {
				return "", errors.New("duplicate fixture phase")
			}
			phase = strings.TrimPrefix(field, "routerd2_storage_phase=")
		}
	}
	if !dedicated || phase != "first" && phase != "second" {
		return "", errors.New("dedicated storage guest flag and fixed phase are required")
	}
	return phase, nil
}

func initializeAndExec(phase string) error {
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: runtimeDir, StateDir: stateDir}); err != nil {
		return err
	}
	if err := requireNoNIC(); err != nil {
		return err
	}
	if err := kmod.Load([]string{"virtio_pci", "virtio_blk", "ext4"}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	volume, err := storage.Mount(ctx, storage.Options{Platform: "appliance", Device: devicePath, Target: stateDir, WaitTimeout: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("fixture seed mount: %w (%s)", err, describeStoragePaths())
	}
	if !volume.Mounted || volume.AlreadyMounted {
		return errors.New("fixture did not freshly mount its dedicated data disk")
	}
	if phase == "first" {
		if err := seedDisk(ctx); err != nil {
			return err
		}
	} else if err := verifyUnconfirmedDisk(ctx); err != nil {
		return err
	}
	if err := writeBootConfig(); err != nil {
		return err
	}
	// The helper must not lend its mounted volume to product startup. Requiring
	// a real unmount makes missing production storage wiring fail this gate.
	unix.Sync()
	if err := unix.Unmount(stateDir, 0); err != nil {
		return fmt.Errorf("unmount seeded fixture disk before product boot: %w", err)
	}
	if _, err := os.Stat(witnessPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("data witness remained visible after seed unmount")
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		return errors.New("underlying product mountpoint is not empty")
	}
	fmt.Printf("%s_SETUP phase=%s seed_unmounted=true uuid=%s\n", marker, phase, volume.UUID)
	probe, err := os.StartProcess("/init", []string{"storage-probe", "probe"}, &os.ProcAttr{
		Dir: "/", Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return err
	}
	if err := probe.Release(); err != nil {
		return err
	}
	return unix.Exec("/test/bin/baseline/routerd", []string{"routerd", "boot", "--file", bootPath}, os.Environ())
}

func describeStoragePaths() string {
	var result []string
	for _, path := range []string{"/", "/dev", devicePath, "/var", "/var/lib", stateDir} {
		var metadata unix.Stat_t
		if err := unix.Lstat(path, &metadata); err != nil {
			result = append(result, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		result = append(result, fmt.Sprintf("%s: uid=%d mode=%#o", path, metadata.Uid, metadata.Mode))
	}
	return strings.Join(result, "; ")
}

func seedDisk(ctx context.Context) error {
	if _, err := os.Lstat(witnessPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("first boot disk already has a fixture witness")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer clear(private)
	if err := config.AtomicJSON(trustPath, map[string]string{release.KeyID(public): base64.StdEncoding.EncodeToString(public)}); err != nil {
		return err
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
	baseline, err := stagePair(ctx, store, private, "baseline", baselineID, 1)
	if err != nil {
		return fmt.Errorf("stage baseline: %w", err)
	}
	trial, err := stagePair(ctx, store, private, "trial", trialID, 2)
	if err != nil {
		return fmt.Errorf("stage trial: %w", err)
	}
	state, err := store.Initialize(ctx, baseline)
	if err != nil {
		return err
	}
	state, err = store.Prepare(ctx, trial, state.Generation, release.PrepareOptions{MaxAttempts: 1})
	if err != nil {
		return err
	}
	if state.Active == nil || *state.Active != baseline || state.Pending == nil || state.Pending.Release != trial || state.Attempt != nil {
		return errors.New("fixture failed to queue baseline-to-trial update")
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer clear(clientKey)
	if err := writePrivate(clientPath, clientKey); err != nil {
		return err
	}
	var sentinel [32]byte
	if _, err := rand.Read(sentinel[:]); err != nil {
		return err
	}
	if err := writePrivate(sentinelPath, sentinel[:]); err != nil {
		return err
	}
	if err := config.AtomicJSON(seedPath, config.Document{SchemaVersion: config.SchemaVersion, Network: network.Desired{Interfaces: []network.Interface{{Name: "lo"}}}}); err != nil {
		return err
	}
	bootID, err := kernelBootID()
	if err != nil {
		return err
	}
	return config.AtomicJSON(witnessPath, diskWitness{Baseline: baseline, Trial: trial, FirstBootID: bootID})
}

func stagePair(ctx context.Context, store *release.Store, key ed25519.PrivateKey, directory, identity string, sequence uint64) (release.Ref, error) {
	first, err := os.Open(filepath.Join("/test/bin", directory, "routerd"))
	if err != nil {
		return release.Ref{}, err
	}
	defer first.Close()
	second, err := os.Open(filepath.Join("/test/bin", directory, "routerd-modules"))
	if err != nil {
		return release.Ref{}, err
	}
	defer second.Close()
	path := filepath.Join(stateDir, directory+".tar")
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return release.Ref{}, err
	}
	_, err = release.WriteBundle(ctx, output, release.BundleOptions{ReleaseID: identity, Sequence: sequence, GOOS: "linux", GOARCH: "amd64"}, key, first, second)
	if err == nil {
		err = output.Sync()
	}
	err = errors.Join(err, output.Close())
	if err != nil {
		return release.Ref{}, err
	}
	ref, err := store.StageFile(ctx, path)
	if err == nil {
		err = os.Remove(path)
	}
	return ref, err
}

func verifyUnconfirmedDisk(ctx context.Context) error {
	witness, err := readWitness()
	if err != nil {
		return err
	}
	bootID, err := kernelBootID()
	if err != nil || witness.FirstBootID == "" || witness.FirstBootID == bootID || witness.Revision != 2 {
		return errors.New("second boot lacks completed evidence from a distinct first kernel boot")
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
	if state.Generation != witness.ReleaseGeneration || state.Operation != "begin-boot" || state.Active == nil || *state.Active != witness.Trial || state.Pending == nil || state.Pending.MaxAttempts != 1 || state.Attempt == nil || state.Attempt.BootID != witness.FirstBootID || state.Attempt.Number != 1 {
		return errors.New("unconfirmed trial journal did not survive the first poweroff")
	}
	return nil
}

func writeBootConfig() error {
	client, err := clientSigner()
	if err != nil {
		return err
	}
	b := runtimeconfig.Default()
	b.Platform, b.Config, b.RuntimeDir, b.StateDir = "appliance", seedPath, runtimeDir, stateDir
	b.ModulePath = "/must-use-verified-module-descriptor"
	b.KernelModules = []string{"virtio_pci", "virtio_blk", "ext4", "nf_tables", "nft_ct", "nft_chain_nat", "nft_masq", "ip6_tunnel"}
	b.StateVolume = &runtimeconfig.StateVolume{Device: devicePath, WaitTimeoutSeconds: 10}
	b.SSH = runtimeconfig.SSH{Listen: sshAddress, User: "admin", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(client.PublicKey()))}}
	// The probe powers off well before the continuous-health confirmation
	// window. The next real kernel boot must spend the pending attempt.
	b.Release = &runtimeconfig.Release{Root: storeRoot, TrustFile: trustPath, ReadyTimeoutSeconds: 900, StableSeconds: 300}
	if err := b.Validate(); err != nil {
		return err
	}
	return config.AtomicJSON(bootPath, b)
}

func clientSigner() (ssh.Signer, error) {
	key, err := os.ReadFile(clientPath)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private fixture client key")
	}
	return ssh.NewSignerFromKey(append(ed25519.PrivateKey(nil), key...))
}

func readWitness() (diskWitness, error) {
	data, err := os.ReadFile(witnessPath)
	if err != nil {
		return diskWitness{}, err
	}
	var witness diskWitness
	err = json.Unmarshal(data, &witness)
	return witness, err
}

func kernelBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data)), err
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	clear(data)
	return hex.EncodeToString(digest[:]), nil
}

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func requireNoNIC() error {
	interfaces, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return err
	}
	for _, device := range interfaces {
		if device.Name() == "lo" {
			continue
		}
		if device.Name() == "ip6tnl0" {
			kind, err := os.ReadFile("/sys/class/net/ip6tnl0/type")
			if err == nil && strings.TrimSpace(string(kind)) == fmt.Sprint(unix.ARPHRD_TUNNEL6) {
				if _, err := os.Stat("/sys/class/net/ip6tnl0/device"); errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
		}
		return fmt.Errorf("unexpected guest NIC %s", device.Name())
	}
	return nil
}

func poweroff() {
	_ = unix.IoctlSetInt(int(os.Stdout.Fd()), unix.TCSBRK, 1)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Println(marker+"_FAIL: guest poweroff:", err)
	}
	for {
		time.Sleep(time.Second)
	}
}
