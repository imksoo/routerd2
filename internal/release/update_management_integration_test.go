//go:build linux && integration

package release_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/management"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"golang.org/x/crypto/ssh"
)

// Real SSH, UDS, signed product executables and target configuration validation.
// Initial confirmed selection is arranged through the store; this does not
// claim a kernel reboot, automatic trial confirmation or packet forwarding.
func TestBuiltRuntimeSSHRollback(t *testing.T) {
	binaryDir, id := os.Getenv("ROUTERD2_RELEASE_BUILT_DIR"), os.Getenv("ROUTERD2_RELEASE_BUILT_ID")
	if binaryDir == "" || id == "" {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("actual built runtime directory and release ID are required")
		}
		t.Skip("set ROUTERD2_RELEASE_BUILT_DIR and ROUTERD2_RELEASE_BUILT_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pub)
	releaseDir := filepath.Join(t.TempDir(), "releases")
	root, err := release.Open(release.Options{Root: releaseDir, TrustedKeys: map[string]ed25519.PublicKey{hex.EncodeToString(digest[:]): pub}, MaxStoredReleases: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		root.Close()
		_ = filepath.WalkDir(releaseDir, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	files := map[string]*os.File{}
	var artifacts []release.Artifact
	for _, name := range []string{"routerd", "routerd-modules", "routerd-logging"} {
		f, err := os.Open(filepath.Join(binaryDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		// Signed artifact names need not match their executable commands.
		artifact := name
		if name == "routerd-modules" {
			artifact = "routerd-services"
		}
		files[artifact] = f
		artifacts = append(artifacts, release.Artifact{Name: artifact, Kind: "native", Command: name})
	}
	var roles []release.RoleBinding
	for role, artifact := range runtimeconfig.NativeWorkers("routerd-services") {
		if role == "logging" {
			artifact = "routerd-logging"
		}
		roles = append(roles, release.RoleBinding{Role: role, Artifact: artifact})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	bundle := func(sequence uint64) *os.File {
		t.Helper()
		file, err := os.CreateTemp(t.TempDir(), "runtime-*.tar")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		_, err = release.WriteBundle(ctx, file, release.BundleOptions{ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 5, Entrypoint: "routerd", Artifacts: artifacts, Roles: roles}, key, files)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		return file
	}
	stage := func(sequence uint64) release.Ref {
		t.Helper()
		ref, err := root.Stage(ctx, bundle(sequence))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	previous, current := stage(1), stage(2)
	unused := stage(3)
	state, err := root.Initialize(ctx, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = root.Prepare(ctx, current, state.Generation, release.PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	selected, err := root.BeginBoot(ctx, "ssh-rollback-arranged-boot")
	if err != nil {
		t.Fatal(err)
	}
	selected.Image.Close()
	state, err = root.Confirm(ctx, selected.State.Attempt.BootID, selected.State.Attempt.Token, current.Digest)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	if err := config.AtomicJSON(seed, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	cfg, committed, err := config.Open(filepath.Join(dir, "state"), seed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := control.NewWithRelease(cfg, committed, &guardNetwork{}, root, current.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	defer stop()
	ready, stopped := make(chan struct{}), make(chan error, 1)
	go func() {
		stopped <- localapi.Serve(serving, filepath.Join(dir, control.SocketName), engine.Handler(), func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-stopped:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() {
		stop()
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	view := startUpdateView(t, ctx, dir)
	sshKey, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	sshReady, sshStopped := make(chan struct{}), make(chan error, 1)
	go func() {
		sshStopped <- management.Serve(serving, runtimeconfig.SSH{Listen: address, User: "operator", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(sshKey.PublicKey()))}}, dir, filepath.Join(dir, "ssh"), func() { close(sshReady) })
	}()
	select {
	case <-sshReady:
	case err := <-sshStopped:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() {
		stop()
		if err := <-sshStopped; err != nil {
			t.Error(err)
		}
	}()
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{User: "operator", Auth: []ssh.AuthMethod{ssh.PublicKeys(sshKey)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	command := func(name string, input []byte, wantFailure bool) release.State {
		t.Helper()
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		session.Stdin = bytes.NewReader(input)
		var output, diagnostic bytes.Buffer
		session.Stdout, session.Stderr = &output, &diagnostic
		err = session.Run(name)
		if wantFailure {
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			return release.State{}
		}
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, diagnostic.String())
		}
		var result release.State
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	request := control.ReleaseRollbackRequest{ExpectedGeneration: state.Generation, ExpectedRevision: committed.Revision}
	stale := request
	stale.ExpectedRevision++
	raw, _ := json.Marshal(stale)
	command("update rollback", raw, true)
	stale = request
	stale.ExpectedGeneration--
	raw, _ = json.Marshal(stale)
	command("update rollback", raw, true)
	unchanged := command("update status", nil, false)
	if unchanged.Generation != state.Generation || unchanged.Pending != nil {
		t.Fatal("refused rollback changed selection")
	}
	checkUpdateView(t, ctx, view, unchanged, "確定版を選択中", current.Digest)
	raw, _ = json.Marshal(request)
	pending := command("update rollback", raw, false)
	if pending.Active == nil || *pending.Active != current || pending.Pending == nil || pending.Pending.Release != previous || pending.Operation != "prepare-rollback" || pending.HighWatermark != current.Sequence {
		t.Fatal("rollback did not queue the actual previous runtime while preserving current selection")
	}
	snapshot := pending.Pending.Startup.Snapshot
	if snapshot.StoreID != committed.StoreID || snapshot.Revision != committed.Revision {
		t.Fatal("rollback did not preserve current configuration")
	}
	checkUpdateView(t, ctx, view, pending, "次回起動待ち", current.Digest)
	saved, err := cfg.Load()
	if err != nil || saved.Revision != committed.Revision {
		t.Fatal("preparation changed running configuration", err)
	}
	command("update rollback", raw, true)
	raw, _ = json.Marshal(control.ReleaseCancelRequest{ExpectedGeneration: pending.Generation})
	canceled := command("update cancel", raw, false)
	if canceled.Pending != nil || canceled.Active == nil || *canceled.Active != current || canceled.ConfigHighWatermark != pending.ConfigHighWatermark {
		t.Fatal("cancel changed active selection or reserved authority")
	}
	checkUpdateView(t, ctx, view, canceled, "確定版を選択中", current.Digest)
	prune := func(request control.ReleasePruneRequest, wantFailure bool) release.CleanupResult {
		t.Helper()
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		session.Stdin = bytes.NewReader(raw)
		var output, diagnostic bytes.Buffer
		session.Stdout, session.Stderr = &output, &diagnostic
		err = session.Run("update prune")
		if wantFailure {
			if err == nil || output.Len() != 0 {
				t.Fatalf("refused prune reported success: %v %s", err, output.String())
			}
			return release.CleanupResult{}
		}
		if err != nil {
			t.Fatalf("SSH prune: %v: %s", err, diagnostic.String())
		}
		var result release.CleanupResult
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	// A bounded store must be recoverable through the same remote management
	// path without deleting the running runtime or its rollback target.
	nextBundle := bundle(4)
	if _, err := root.Stage(ctx, nextBundle); !errors.Is(err, release.ErrCapacity) {
		t.Fatalf("full runtime store: %v", err)
	}
	prune(control.ReleasePruneRequest{ExpectedGeneration: canceled.Generation - 1, ReleaseDigests: []string{unused.Digest}}, true)
	for _, protected := range []release.Ref{current, previous} {
		prune(control.ReleasePruneRequest{ExpectedGeneration: canceled.Generation, ReleaseDigests: []string{protected.Digest}}, true)
	}
	// Interrupt the real local CLI during removal, then recover through the
	// already running SSH management path. Remove old journal entries first so
	// the fifth unlink belongs to the unselected runtime, not its journal.
	journals := prune(control.ReleasePruneRequest{ExpectedGeneration: canceled.Generation}, false)
	if len(journals.RemovedGenerations) == 0 || len(journals.RemovedReleases) != 0 {
		t.Fatal("journal cleanup did not preserve stored runtimes", journals)
	}
	trust := filepath.Join(t.TempDir(), "trust.json")
	if err := config.AtomicJSON(trust, map[string][]byte{release.KeyID(pub): pub}); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "prune.strace")
	interrupted := exec.CommandContext(ctx, "strace", "-f", "-qq", "-o", trace,
		"-e", "trace=unlinkat,fsync", "-e", "signal=SIGKILL", "-e", "inject=unlinkat:signal=SIGKILL:when=5",
		filepath.Join(binaryDir, "routerd"), "release", "prune", "--root", releaseDir,
		"--trust-file", trust, "--target-arch", runtime.GOARCH,
		"--expected-generation", fmt.Sprint(canceled.Generation), "--digest", unused.Digest)
	interrupted.Env = append(os.Environ(), "GOMAXPROCS=1")
	output, interruptionErr := interrupted.CombinedOutput()
	traceBytes, traceErr := os.ReadFile(trace)
	if interruptionErr == nil || traceErr != nil || !strings.Contains(string(traceBytes), "killed by SIGKILL") {
		t.Fatalf("prune was not interrupted: %v %v %s %s", interruptionErr, traceErr, output, traceBytes)
	}
	t.Logf("local prune interrupted during deletion: %s", traceBytes)
	cleanup := prune(control.ReleasePruneRequest{ExpectedGeneration: canceled.Generation, ReleaseDigests: []string{unused.Digest}}, false)
	if !slices.Equal(cleanup.RemovedReleases, []string{unused.Digest}) {
		t.Fatal("SSH prune did not report the removed unused runtime and old journal generations", cleanup)
	}
	afterCleanup := command("update status", nil, false)
	if afterCleanup.Generation != canceled.Generation || afterCleanup.Active == nil || *afterCleanup.Active != current || afterCleanup.Previous == nil || *afterCleanup.Previous != previous || afterCleanup.HighWatermark != canceled.HighWatermark {
		t.Fatal("cleanup changed the selected runtimes or rollback fence")
	}
	if _, err := nextBundle.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	nextRef, err := root.Stage(ctx, nextBundle)
	if err != nil {
		t.Fatal("explicit SSH cleanup did not free capacity for the next signed runtime", err)
	}
	t.Log("SSH prune: stale generation and protected runtimes refused; local CLI killed during unused runtime removal; SSH retry reclaimed capacity; unchanged selection/watermarks and next signed runtime staging succeeded")
	t.Log("SSH rollback: verified previous product compiler accepted current configuration; stale revision/generation and duplicate rejected; active runtime/configuration unchanged; cancellation preserved watermarks")
	// Arrange actual journal transitions for the read-only console. The control
	// handler remains on its original executable; this is not a reboot trial.
	raw, _ = json.Marshal(control.ReleasePrepareRequest{ExpectedGeneration: afterCleanup.Generation, ExpectedRevision: committed.Revision, Release: nextRef})
	prepared := command("update prepare", raw, false)
	prune(control.ReleasePruneRequest{ExpectedGeneration: prepared.Generation, ReleaseDigests: []string{nextRef.Digest}}, true)
	trial, err := root.BeginBoot(ctx, "update-view-arranged-boot")
	if err != nil {
		t.Fatal(err)
	}
	trial.Image.Close()
	checkUpdateView(t, ctx, view, trial.State, "試行中（正常確定待ち）", current.Digest)
	recovered, err := root.FailBoot(ctx, trial.State.Attempt.BootID, trial.State.Attempt.Token, "recorded trial failure <unconfirmed>")
	if err != nil {
		t.Fatal(err)
	}
	checkUpdateView(t, ctx, view, recovered, "確定版を選択中", current.Digest)

	// A crashed stage can leave an empty manifest before it writes any payload.
	// Recover the occupied store slot through SSH, without repairing its files.
	prune(control.ReleasePruneRequest{ExpectedGeneration: recovered.Generation, ReleaseDigests: []string{nextRef.Digest}}, false)
	freshBundle := bundle(5)
	stageTrace := filepath.Join(t.TempDir(), "stage.strace")
	staging := exec.CommandContext(ctx, "strace", "-f", "-qq", "-o", stageTrace,
		"-e", "trace=openat,write,fsync", "-e", "signal=SIGKILL", "-e", "inject=write:signal=SIGKILL:when=2",
		filepath.Join(binaryDir, "routerd"), "release", "stage", "--root", releaseDir,
		"--trust-file", trust, "--target-arch", runtime.GOARCH, "--file", freshBundle.Name())
	staging.Env = append(os.Environ(), "GOMAXPROCS=1")
	stageOutput, stageErr := staging.CombinedOutput()
	stageTraceBytes, stageTraceErr := os.ReadFile(stageTrace)
	if stageErr == nil || stageTraceErr != nil || !strings.Contains(string(stageTraceBytes), "killed by SIGKILL") {
		t.Fatalf("stage was not interrupted: %v %v %s %s", stageErr, stageTraceErr, stageOutput, stageTraceBytes)
	}
	for _, line := range strings.Split(string(stageTraceBytes), "\n") {
		if strings.Contains(line, "manifest.json") || strings.Contains(line, "write(") || strings.Contains(line, "killed by SIGKILL") {
			t.Logf("interrupted stage: %s", line)
		}
	}
	orphans, err := filepath.Glob(filepath.Join(releaseDir, "releases", ".stage-*"))
	if err != nil || len(orphans) != 1 {
		t.Fatalf("expected one interrupted stage: %v %v", orphans, err)
	}
	metadata, err := os.ReadFile(filepath.Join(orphans[0], "manifest.json"))
	if err != nil || len(metadata) != 0 {
		t.Fatalf("stage did not stop before writing manifest data: %v %q", err, metadata)
	}
	if _, err := root.Stage(ctx, freshBundle); !errors.Is(err, release.ErrCapacity) {
		t.Fatalf("interrupted stage did not occupy the store's last slot: %v", err)
	}
	cleaned := prune(control.ReleasePruneRequest{ExpectedGeneration: recovered.Generation}, false)
	if !slices.Equal(cleaned.RemovedReleases, []string{filepath.Base(orphans[0])}) {
		t.Fatal("SSH cleanup did not remove the incomplete stage", cleaned)
	}
	afterStageCleanup := command("update status", nil, false)
	if afterStageCleanup.Generation != recovered.Generation || afterStageCleanup.Active == nil || *afterStageCleanup.Active != current || afterStageCleanup.HighWatermark != recovered.HighWatermark {
		t.Fatal("incomplete stage cleanup changed confirmed selection or publisher fence")
	}
	if _, err := freshBundle.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stage(ctx, freshBundle); err != nil {
		t.Fatal("SSH cleanup did not recover staging capacity", err)
	}
	t.Log("SSH recovery: interrupted stage left empty manifest; full store refused next stage; SSH cleanup removed only the abandoned stage; confirmed selection and publisher fence preserved; next signed stage succeeded")
}
