// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// This initializer and probe exist only in the no-NIC integration image. They
// are never installed in a production appliance or exposed by its CLI.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/management"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeabi"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"golang.org/x/sys/unix"
)

const (
	marker      = "ROUTERD_RELEASE_BOOT"
	guestFlag   = "routerd2_release_boot_fixture=1"
	baselineID  = "release-gate-baseline"
	trialID     = "release-gate-trial"
	storeRoot   = "/var/lib/routerd-releases"
	trustPath   = "/etc/routerd/release-keys.json"
	bootPath    = "/etc/routerd/boot.json"
	witnessPath = "/test/witness.json"
	runtimeDir  = "/run/routerd"
	stateDir    = "/var/lib/routerd"
)

var errLivePreflightFence = errors.New("preflight live configuration fence mismatch")

type witness struct {
	Baseline release.Ref `json:"baseline"`
	Trial    release.Ref `json:"trial"`
	BootID   string      `json:"boot_id"`
}

func main() {
	probe := len(os.Args) == 2 && os.Args[1] == "probe"
	if (!probe && (len(os.Args) != 1 || os.Getpid() != 1)) || (probe && os.Getpid() <= 1) {
		fmt.Fprintln(os.Stderr, marker+"_FAIL: fixture requires its dedicated guest init or probe")
		os.Exit(1)
	}
	// Validate the dedicated kernel command line before mounting other kernel
	// interfaces or allowing the fixture's poweroff path. A host invocation is
	// rejected and never calls reboot.
	if !probe {
		if _, err := os.Stat("/proc/cmdline"); os.IsNotExist(err) {
			if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
				abortBeforeGuest(fmt.Errorf("mount fixture proc: %w", err))
			}
		}
	}
	if err := verifyGuest(); err != nil {
		abortBeforeGuest(err)
	}
	var err error
	if probe {
		err = runProbe()
	} else {
		err = initializeAndExec()
	}
	if err != nil {
		fmt.Println(marker+"_FAIL:", err)
	} else {
		// initializeAndExec cannot return successfully: its PID 1 is replaced
		// by the real baseline routerd, which selects and execs the trial.
		fmt.Println(marker + "_PASS")
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

func verifyGuest() error {
	if os.Geteuid() != 0 {
		return errors.New("fixture requires guest root")
	}
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(data)) {
		if field == guestFlag {
			return nil
		}
	}
	return errors.New("dedicated release-boot kernel flag is missing")
}

func initializeAndExec() error {
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: runtimeDir, StateDir: stateDir}); err != nil {
		return err
	}
	if err := requireNoNIC(); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/routerd", 0755); err != nil {
		return err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeJSON(trustPath, map[string]string{release.KeyID(publicKey): base64.StdEncoding.EncodeToString(publicKey)}, 0600); err != nil {
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
	ctx := context.Background()
	base, err := stagePair(ctx, store, privateKey, "baseline", baselineID, 1)
	if err != nil {
		return fmt.Errorf("stage baseline: %w", err)
	}
	next, err := stagePair(ctx, store, privateKey, "trial", trialID, 2)
	if err != nil {
		return fmt.Errorf("stage trial: %w", err)
	}
	state, err := store.Initialize(ctx, base)
	if err != nil {
		return err
	}
	state, err = store.Prepare(ctx, next, state.Generation, release.PrepareOptions{MaxAttempts: 1})
	if err != nil {
		return err
	}
	if state.Active == nil || *state.Active != base || state.Pending == nil || state.Pending.Release != next || state.Attempt != nil {
		return errors.New("fixture did not queue a baseline-to-trial update")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	if err := writeJSON(witnessPath, witness{Baseline: base, Trial: next, BootID: strings.TrimSpace(string(bootID))}, 0600); err != nil {
		return err
	}
	b := runtimeconfig.Default()
	b.Platform = "appliance"
	b.Config = "/etc/routerd/config.json"
	b.RuntimeDir = runtimeDir
	b.StateDir = stateDir
	// The actual boot integration must replace this path with the selected,
	// verified modules FD before supervisor startup.
	b.Workers = runtimeconfig.NativeWorkers("/must-use-verified-role-descriptor")
	b.KernelModules = []string{"nf_tables", "nft_ct", "nft_chain_nat", "nft_masq", "ip6_tunnel"}
	b.Release = &runtimeconfig.Release{Root: storeRoot, TrustFile: trustPath, ReadyTimeoutSeconds: 90, StableSeconds: 2}
	if err := b.Validate(); err != nil {
		return err
	}
	if err := writeJSON(bootPath, b, 0600); err != nil {
		return err
	}
	if err := writeJSON(b.Config, config.Document{SchemaVersion: config.SchemaVersion}, 0600); err != nil {
		return err
	}
	image, err := store.Verify(ctx, base)
	if err != nil {
		return err
	}
	defer image.Close()
	if err := store.Close(); err != nil {
		return err
	}
	fmt.Printf("%s_SETUP baseline=%s trial=%s generation=%d\n", marker, base.ReleaseID, next.ReleaseID, state.Generation)
	probe, err := os.StartProcess("/init", []string{"release-boot-probe", "probe"}, &os.ProcAttr{
		Dir: "/", Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return fmt.Errorf("start independent witness: %w", err)
	}
	// The real supervisor will own reaping after exec. There is no competing
	// os/exec Wait goroutine in its process.
	if err := probe.Release(); err != nil {
		return err
	}
	return unix.Exec(fmt.Sprintf("/proc/self/fd/%d", image.Entrypoint().Fd()),
		[]string{"routerd", "boot", "--file", bootPath}, os.Environ())
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
	path := filepath.Join("/test", directory+".tar")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return release.Ref{}, err
	}
	logger, err := os.Open(filepath.Join("/test/bin", directory, "routerd-logging"))
	if err != nil {
		return release.Ref{}, err
	}
	defer logger.Close()
	noticesPath := filepath.Join("/test", directory+"-notices.txt")
	if err := os.WriteFile(noticesPath, []byte("test-only runtime catalog fixture notices\n"), 0600); err != nil {
		return release.Ref{}, err
	}
	notices, err := os.Open(noticesPath)
	if err != nil {
		return release.Ref{}, err
	}
	defer notices.Close()
	runtimeOptions := release.BundleOptions{ReleaseID: identity, Sequence: sequence, GOOS: "linux", GOARCH: "amd64", BootABI: 1, WorkerABI: 5, Entrypoint: "supervisor", Artifacts: []release.Artifact{{Name: "supervisor", Kind: "native", Command: "routerd"}, {Name: "services", Kind: "native", Command: "routerd-modules"}, {Name: "logger", Kind: "native", Command: "routerd-logging"}, {Name: "notices", Kind: "data"}}}
	roles := runtimeconfig.NativeWorkers("services")
	roles["logging"] = "logger"
	names := make([]string, 0, len(roles))
	for role := range roles {
		names = append(names, role)
	}
	sort.Strings(names)
	for _, role := range names {
		runtimeOptions.Roles = append(runtimeOptions.Roles, release.RoleBinding{Role: role, Artifact: roles[role]})
	}
	fmt.Println("ROUTERD_RUNTIME_CATALOG_FIXTURE artifacts=4 native=3 data=1 native_commands=independently_built")
	_, err = release.WriteBundle(ctx, out, runtimeOptions, key, map[string]*os.File{"supervisor": first, "services": second, "logger": logger, "notices": notices})
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return release.Ref{}, err
	}
	ref, err := store.StageFile(ctx, path)
	if err != nil {
		return release.Ref{}, err
	}
	if err := os.Remove(path); err != nil {
		return release.Ref{}, err
	}
	return ref, nil
}

func runProbe() error {
	raw, err := os.ReadFile(witnessPath)
	if err != nil {
		return err
	}
	var expected witness
	if err := json.Unmarshal(raw, &expected); err != nil {
		return err
	}
	keys, err := release.LoadTrustFile(trustPath)
	if err != nil {
		return err
	}
	var store *release.Store
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("real PID 1 did not confirm the trial: %w", last)
		case <-timer.C:
			err := observe(ctx, &store, keys, expected)
			if err == nil {
				return verifyLiveCommitFreeze(ctx, store)
			}
			last = err
			// Leave an actual idle interval after ELF re-verification. An
			// already queued ticker tick could otherwise let the observer
			// monopolize the store on a slow emulated CPU.
			timer.Reset(time.Second)
		}
	}
}

// This deliberately uses the existing raw journal primitives, not an operator
// prepare API. The running signed PID 1 and real control/network workers must
// enforce the fence before a second paired boot is enabled in the product.
func verifyLiveCommitFreeze(ctx context.Context, store *release.Store) error {
	client := localapi.NewClient(filepath.Join(runtimeDir, control.SocketName))
	var initial control.State
	if err := client.Do(ctx, "GET", "/v1/config", nil, &initial); err != nil {
		return err
	}
	if initial.ConfigOmitted || initial.Revision != 1 {
		return errors.New("live fence fixture requires the original confirmed seed")
	}
	request := control.CommitRequest{ExpectedRevision: initial.Revision, RequestID: "live-fence-before-prepare", Config: initial.Config}
	var committed control.State
	if err := client.Do(ctx, "POST", "/v1/config/commit", request, &committed); err != nil {
		return fmt.Errorf("commit before paired prepare: %w", err)
	}
	if committed.Revision != 2 {
		return errors.New("first live commit did not advance once")
	}
	var snapshot config.SnapshotRef
	if err := client.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: 2}, &snapshot); err != nil {
		return err
	}
	state, err := store.Status(ctx)
	if err != nil {
		return err
	}
	selection := configref.Selection{Generation: 10, ExpectedRevision: 2, Snapshot: snapshot}
	options := release.PrepareOptions{Startup: &selection, KnownGood: &snapshot}
	preflight := func(expectedRevision uint64, reference config.SnapshotRef, race bool) release.Preflight {
		return func(call context.Context, current release.State, target *release.Image) error {
			if current.Active == nil || state.Active == nil || *current.Active != *state.Active {
				return errors.New("preflight current runtime changed")
			}
			if current.Previous == nil || target.Release != *current.Previous {
				return errors.New("preflight did not receive the actual rollback target")
			}
			if _, err := target.Entrypoint().Stat(); err != nil {
				return fmt.Errorf("preflight target descriptor: %w", err)
			}
			if race {
				request := control.CommitRequest{ExpectedRevision: 2, RequestID: "live-preflight-race", Config: initial.Config}
				err := client.Do(call, "POST", "/v1/config/commit", request, nil)
				var busy *localapi.HTTPError
				if !errors.As(err, &busy) || busy.StatusCode != 503 {
					return fmt.Errorf("live commit entered locked preflight: %v", err)
				}
			}
			var live control.State
			if err := client.Do(call, "GET", "/v1/config", nil, &live); err != nil {
				return err
			}
			if live.StoreID != reference.StoreID || live.Revision != expectedRevision || live.Confirmation != nil {
				return errLivePreflightFence
			}
			// Inspect both actual startup inputs with the same read-only checks
			// adoption will run. Target compiler/capability and other state
			// families remain separate qualification gates.
			cfg := &config.Store{Dir: stateDir}
			candidate := configref.Selection{Generation: selection.Generation, ExpectedRevision: expectedRevision, Snapshot: reference}
			checked, err := cfg.InspectStartup(candidate)
			if err != nil {
				return err
			}
			if checked.AlreadyAdopted || checked.Recovered {
				return errors.New("preflight unexpectedly consumed candidate")
			}
			candidateChecked := checked
			fallback := configref.Selection{Generation: selection.Generation + 1, ExpectedRevision: expectedRevision, Snapshot: snapshot, Recovery: &configref.Recovery{FailedGeneration: selection.Generation, FailedSnapshot: reference}}
			checked, err = cfg.InspectStartup(fallback)
			if err != nil {
				return err
			}
			if checked.AlreadyAdopted || checked.Recovered {
				return errors.New("preflight unexpectedly consumed fallback")
			}
			workerJournal, err := control.NewNetworkClient(runtimeDir).InspectJournal(call)
			if err != nil {
				return err
			}
			diskJournal, err := network.InspectJournal(call, filepath.Join(stateDir, "network.json"))
			if err != nil {
				return err
			}
			if !workerJournal.Present || workerJournal.Version != network.JournalVersion || workerJournal.Status != "applied" || diskJournal != workerJournal {
				return errors.New("preflight network journal observation differs from the actual writer file")
			}
			identity, err := management.InspectHostKey(call, filepath.Join(stateDir, management.HostKeyName))
			if err != nil {
				return err
			}
			// SSH is disabled in this fixture. Explicitly observed absence is
			// valid; inspection must not create a key in order to report one.
			if identity.Present {
				return errors.New("disabled SSH fixture unexpectedly initialized an identity")
			}
			var eventFormat events.FormatInspection
			if err := client.Do(call, "GET", "/v1/event-store/format", nil, &eventFormat); err != nil {
				return err
			}
			activeABI := runtimeabi.Current()
			for _, input := range []config.StartupInspection{candidateChecked, checked} {
				facts := runtimeabi.Facts{Configuration: uint32(input.Snapshot.Config.SchemaVersion), ConfigurationContract: activeABI.ConfigurationContract,
					State: []runtimeabi.StateVersion{
						{Name: "configuration", Version: config.JournalVersion},
						{Name: "events", Version: uint32(eventFormat.Version)},
						{Name: "network", Version: uint32(workerJournal.Version)},
						{Name: "release", Version: uint32(current.Version)},
						{Name: "snapshot", Version: uint32(input.Snapshot.Version)},
						{Name: "ssh_identity", Version: uint32(identity.Version)},
					}}
				if err := runtimeabi.CheckTransition(activeABI, target.Manifest.RuntimeABI, facts); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if _, err := store.PrepareRollbackChecked(ctx, state.Generation, options, preflight(3, snapshot, false)); !errors.Is(err, errLivePreflightFence) {
		return fmt.Errorf("stale live configuration did not reach its intended preflight refusal: %v", err)
	}
	missing := snapshot
	missing.Digest = strings.Repeat("0", 64)
	if _, err := store.PrepareRollbackChecked(ctx, state.Generation, options, preflight(2, missing, false)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("missing snapshot did not reach its intended preflight refusal: %v", err)
	}
	unchangedRelease, err := store.Status(ctx)
	if err != nil || unchangedRelease.Generation != state.Generation || unchangedRelease.ConfigHighWatermark != state.ConfigHighWatermark || unchangedRelease.Pending != nil {
		return fmt.Errorf("failed preflight changed release authority: %v", err)
	}
	pending, err := store.PrepareRollbackChecked(ctx, state.Generation, options, preflight(2, snapshot, true))
	if err != nil {
		return fmt.Errorf("prepare raw paired rollback intent: %w", err)
	}
	var replay control.State
	if err := client.Do(ctx, "POST", "/v1/config/commit", request, &replay); err != nil || replay.Revision != 2 {
		return fmt.Errorf("prepared pair lost durable replay (revision %d): %v", replay.Revision, err)
	}
	request.ExpectedRevision, request.RequestID = 2, "live-fence-blocked"
	err = client.Do(ctx, "POST", "/v1/config/commit", request, nil)
	var rejected *localapi.HTTPError
	if !errors.As(err, &rejected) || rejected.StatusCode != 409 {
		return fmt.Errorf("prepared pair allowed a real control commit: %v", err)
	}
	var unchanged control.State
	if err := client.Do(ctx, "GET", "/v1/config", nil, &unchanged); err != nil {
		return err
	}
	if unchanged.Revision != 2 || unchanged.LastRequestID != committed.LastRequestID {
		return errors.New("rejected live commit changed the configuration journal")
	}
	canceled, err := store.CancelPending(ctx, pending.Generation)
	if err != nil {
		return err
	}
	if canceled.ConfigHighWatermark != 11 || canceled.ConfigStoreID != snapshot.StoreID {
		return errors.New("cancel discarded paired authority reservations")
	}
	request.RequestID = "live-fence-after-cancel"
	var final control.State
	if err := client.Do(ctx, "POST", "/v1/config/commit", request, &final); err != nil {
		return fmt.Errorf("cancel did not unfreeze real control: %w", err)
	}
	if final.Revision != 3 || final.StoreID != snapshot.StoreID {
		return errors.New("post-cancel commit did not preserve the live store")
	}
	fmt.Printf("%s_COMMIT_FENCE_WITNESS before=1 committed=2 blocked_status=409 replay=2 after_cancel=3 reserved_generation=11 store_identity=matched preflight_race_status=503 stale_revision=rejected missing_snapshot=rejected target_descriptor=verified candidate_inspected=true fallback_inspected=true network_journal=verified ssh_identity=inspected_absent declared_state_formats=all_six\n", marker)
	return nil
}

func observe(ctx context.Context, store **release.Store, keys map[string]ed25519.PublicKey, expected witness) error {
	var processes supervisor.Status
	var health control.State
	var seen map[string]bool
	return observeWithBudgets(ctx, 5*time.Second, func(ctx context.Context) error {
		if err := requireNoNIC(); err != nil {
			return err
		}
		if err := localapi.NewClient(filepath.Join(runtimeDir, supervisor.SocketName)).Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
			return err
		}
		if processes.PID != 1 || processes.Platform != "appliance" || !processes.Ready || processes.Lifecycle != supervisor.Running || processes.ReleaseID != expected.Trial.ReleaseID {
			return fmt.Errorf("unexpected supervisor readiness: %+v", processes)
		}
		if len(processes.Modules) != 4 {
			return fmt.Errorf("unexpected number of core modules: %d", len(processes.Modules))
		}
		routerdPath := filepath.Join(storeRoot, "releases", expected.Trial.Digest, "supervisor")

		if err := sameExecutable(1, routerdPath); err != nil {
			return err
		}
		seen = make(map[string]bool)
		for _, module := range processes.Modules {
			if !module.Ready || module.PID <= 1 || module.ReleaseID != expected.Trial.ReleaseID {
				return fmt.Errorf("module %s is not ready in selected pair: %+v", module.Name, module)
			}
			artifact := "services"
			if module.Role == "logging" {
				artifact = "logger"
			}
			modulesPath := filepath.Join(storeRoot, "releases", expected.Trial.Digest, artifact)
			if err := sameExecutable(module.PID, modulesPath); err != nil {
				return fmt.Errorf("module %s: %w", module.Name, err)
			}
			if seen[module.Name] {
				return fmt.Errorf("duplicate module %s", module.Name)
			}
			seen[module.Name] = true
			if module.Name != "logging" && !module.Required {
				return fmt.Errorf("core module %s is not required", module.Name)
			}
		}
		for _, name := range []string{"logging", "network", "control", "management"} {
			if !seen[name] {
				return fmt.Errorf("missing core module %s", name)
			}
		}
		if err := localapi.NewClient(filepath.Join(runtimeDir, control.SocketName)).Do(ctx, "GET", "/v1/health", nil, &health); err != nil {
			return err
		}
		if !health.Healthy || health.Revision == 0 || health.Confirmation != nil || health.Services == nil ||
			health.Services.Phase != "ready" || health.Services.Revision != health.Revision || health.Services.RouteIssueCount != 0 {
			return fmt.Errorf("selected release control plane is not healthy: %+v", health)
		}
		return nil
	}, func(ctx context.Context) error {
		// The probe must not acquire the store while PID 1 is doing its initial
		// Open/BeginBoot/exec sequence. Once supervisor is ready, only the trial
		// watcher can mutate it; that watcher explicitly retries ErrBusy.
		if *store == nil {
			opened, err := release.Open(release.Options{Root: storeRoot, TrustedKeys: keys, TargetOS: "linux", TargetArch: "amd64"})
			if err != nil {
				return err
			}
			*store = opened
		}
		state, err := (*store).Status(ctx)
		if err != nil {
			return err
		}
		if state.Active == nil || *state.Active != expected.Trial || state.Previous == nil || *state.Previous != expected.Baseline ||
			state.Pending != nil || state.Attempt != nil || state.HighWatermark != expected.Trial.Sequence || state.Operation != "confirm" {
			return fmt.Errorf("selected pair has not been automatically confirmed: %+v", state)
		}
		bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil || strings.TrimSpace(string(bootID)) != expected.BootID {
			return errors.New("kernel boot ID changed during same-PID exec")
		}
		var names []string
		for name := range seen {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Printf("%s_WITNESS pid=1 release=%s generation=%d operation=%s revision=%d modules=%s pinned_exec=verified artifact_count=4 native_count=3 data_count=1 role_catalog=verified\n",
			marker, state.Active.ReleaseID, state.Generation, state.Operation, health.Revision, strings.Join(names, ","))
		return nil
	})
}

func observeWithBudgets(ctx context.Context, socketBudget time.Duration, sockets, verifiedRuntime func(context.Context) error) error {
	call, cancel := context.WithTimeout(ctx, socketBudget)
	err := sockets(call)
	cancel()
	if err != nil {
		return fmt.Errorf("worker observation: %w", err)
	}
	if err := verifiedRuntime(ctx); err != nil {
		return fmt.Errorf("verified runtime observation: %w", err)
	}
	return nil
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
		return fmt.Errorf("PID %d does not execute its verified release inode", pid)
	}
	return nil
}

func requireNoNIC() error {
	interfaces, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return err
	}
	for _, device := range interfaces {
		name := device.Name()
		if name == "lo" {
			continue
		}
		if name == "ip6tnl0" {
			// Loading the production ip6_tunnel driver creates this unused
			// kernel fallback tunnel. It has no physical/virtual NIC peer.
			kind, err := os.ReadFile("/sys/class/net/ip6tnl0/type")
			if err == nil && strings.TrimSpace(string(kind)) == fmt.Sprint(unix.ARPHRD_TUNNEL6) {
				if _, err := os.Stat("/sys/class/net/ip6tnl0/device"); errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
		}
		return fmt.Errorf("unexpected network interface in isolated guest: %s", name)
	}
	return nil
}

func writeJSON(path string, value any, mode os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), mode)
}

func poweroff() {
	// Drain the real guest serial console before QEMU powers off. File Sync
	// alone does not wait for the UART.
	_ = unix.IoctlSetInt(int(os.Stdout.Fd()), unix.TCSBRK, 1)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Fprintln(os.Stderr, marker+"_FAIL: guest poweroff:", err)
	}
	for {
		time.Sleep(time.Second)
	}
}
