//go:build linux && integration

package release_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/management"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"golang.org/x/crypto/ssh"
)

// Reuse the signed-product and real SSH/UDS path of TestBuiltRuntimeSSHRollback.
// Arrange saved history before serving management. Network application uses the
// existing guardNetwork; this test does not claim reboot or packet forwarding.
func TestBuiltRuntimeSSHSnapshotDeletionRecoversUpdates(t *testing.T) {
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
	releaseDir := filepath.Join(t.TempDir(), "releases")
	root, err := release.Open(release.Options{Root: releaseDir, TrustedKeys: map[string]ed25519.PublicKey{release.KeyID(pub): pub}})
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
		file, err := os.Open(filepath.Join(binaryDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files[name] = file
		artifacts = append(artifacts, release.Artifact{Name: name, Kind: "native", Command: name})
	}
	var roles []release.RoleBinding
	for role, artifact := range runtimeconfig.NativeWorkers("routerd-modules") {
		roles = append(roles, release.RoleBinding{Role: role, Artifact: artifact})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	stage := func(sequence uint64) release.Ref {
		t.Helper()
		bundle, err := os.CreateTemp(t.TempDir(), "runtime-*.tar")
		if err != nil {
			t.Fatal(err)
		}
		defer bundle.Close()
		manifest, err := release.WriteBundle(ctx, bundle, release.BundleOptions{
			ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH,
			BootABI: 1, WorkerABI: 5, Entrypoint: "routerd", Artifacts: artifacts, Roles: roles,
		}, key, files)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bundle.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		ref, err := root.Stage(ctx, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if sequence == 1 {
			for _, artifact := range manifest.Artifacts {
				t.Logf("signed product: release=%s artifact=%s sha256=%s", id, artifact.Name, artifact.SHA256)
			}
		}
		return ref
	}
	previous, current, next := stage(1), stage(2), stage(3)
	state, err := root.Initialize(ctx, previous)
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
	baseline, err := cfg.Snapshot(committed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	selection := configref.Selection{Generation: 1, ExpectedRevision: committed.Revision, Snapshot: baseline}
	if _, err := root.Prepare(ctx, current, state.Generation, release.PrepareOptions{Startup: &selection, KnownGood: &baseline}); err != nil {
		t.Fatal(err)
	}
	boot, err := root.BeginBoot(ctx, "snapshot-prune-arranged-boot")
	if err != nil {
		t.Fatal(err)
	}
	boot.Image.Close()
	committed, err = cfg.AdoptStartup(selection)
	if err != nil {
		t.Fatal(err)
	}
	state, err = root.ConfirmConfiguration(ctx, boot.State.Attempt.BootID, boot.State.Attempt.Token, current.Digest,
		func(context.Context) (release.ConfigurationConfirmation, error) {
			return release.ConfigurationConfirmation{Adoption: *committed.Startup, Revision: committed.Revision}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var obsolete []configref.Ref
	for count := 1; count < config.MaxSnapshots; count++ {
		ref, err := cfg.Snapshot(committed.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if len(obsolete) < 2 {
			obsolete = append(obsolete, ref)
		}
		committed.Revision++
		if err := cfg.Save(committed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cfg.Snapshot(committed.Revision); !errors.Is(err, config.ErrSnapshotCapacity) {
		t.Fatalf("fixture did not reach snapshot capacity: %v", err)
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
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	sshReady, sshStopped := make(chan struct{}), make(chan error, 1)
	go func() {
		sshStopped <- management.Serve(serving, runtimeconfig.SSH{
			Listen: address, User: "operator", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(signer.PublicKey()))},
		}, dir, filepath.Join(dir, "ssh"), func() { close(sshReady) })
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
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User: "operator", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	command := func(name string, input []byte, wantError string) []byte {
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
		if wantError != "" {
			if err == nil || output.Len() != 0 || !strings.Contains(diagnostic.String(), wantError) {
				t.Fatalf("%s: expected %q, got %v: %s %s", name, wantError, err, output.String(), diagnostic.String())
			}
			return nil
		}
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, diagnostic.String())
		}
		return output.Bytes()
	}
	api := localapi.NewClient(filepath.Join(dir, control.SocketName))
	defer api.HTTP.CloseIdleConnections()
	refuseDelete := func(ref configref.Ref, revision uint64) {
		t.Helper()
		var result configref.Ref
		err := api.Do(ctx, http.MethodPost, "/v1/config/snapshots/delete",
			control.SnapshotDeleteRequest{ExpectedRevision: revision, Snapshot: ref}, &result)
		if !localapi.IsHTTPStatus(err, http.StatusConflict) {
			t.Fatalf("referenced or stale deletion was not refused: %v", err)
		}
		if _, err := cfg.ResolveSnapshot(ref); err != nil {
			t.Fatal("refused deletion removed saved configuration", err)
		}
	}
	refuseDelete(baseline, committed.Revision)
	refuseDelete(obsolete[0], committed.Revision-1)
	prepare := control.ReleasePrepareRequest{ExpectedGeneration: state.Generation, ExpectedRevision: committed.Revision, Release: next}
	raw, _ := json.Marshal(prepare)
	command("update prepare", raw, config.ErrSnapshotCapacity.Error())
	raw, _ = json.Marshal(control.ReleaseRollbackRequest{ExpectedGeneration: state.Generation, ExpectedRevision: committed.Revision})
	command("update rollback", raw, config.ErrSnapshotCapacity.Error())
	t.Logf("full store: %d saved configurations; prepare and rollback refused at revision %d", config.MaxSnapshots, committed.Revision)

	deletion := control.SnapshotDeleteRequest{ExpectedRevision: committed.Revision, Snapshot: obsolete[0]}
	raw, _ = json.Marshal(deletion)
	var removed configref.Ref
	if err := json.Unmarshal(command("snapshot delete", raw, ""), &removed); err != nil || removed != obsolete[0] {
		t.Fatal("SSH deletion did not acknowledge the selected saved configuration", removed, err)
	}
	if _, err := cfg.ResolveSnapshot(obsolete[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted snapshot is still present: %v", err)
	}
	if err := api.Do(ctx, http.MethodPost, "/v1/config/snapshots/delete", deletion, &removed); err != nil || removed != obsolete[0] {
		t.Fatal("lost-acknowledgment retry did not succeed with the same reference", removed, err)
	}
	raw, _ = json.Marshal(prepare)
	var pending release.State
	if err := json.Unmarshal(command("update prepare", raw, ""), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Active == nil || *pending.Active != current || pending.Pending == nil || pending.Pending.Release != next {
		t.Fatal("deletion did not restore preparation without changing the running release")
	}
	// Public preparation saves the current revision. This checks the actual
	// refusal, not independent coverage of the pending-only reference guard.
	refuseDelete(pending.Pending.Startup.Snapshot, committed.Revision)
	refuseDelete(baseline, committed.Revision)
	raw, _ = json.Marshal(control.ReleaseCancelRequest{ExpectedGeneration: pending.Generation})
	if err := json.Unmarshal(command("update cancel", raw, ""), &state); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(control.CommitRequest{
		ExpectedRevision: committed.Revision, RequestID: "snapshot-prune-next-revision", Config: committed.Config,
	})
	var changed control.State
	if err := json.Unmarshal(command("configure", raw, ""), &changed); err != nil {
		t.Fatal(err)
	}
	if changed.Revision != committed.Revision+1 {
		t.Fatal("configuration commit did not establish the next unsaved revision")
	}
	rollback := control.ReleaseRollbackRequest{ExpectedGeneration: state.Generation, ExpectedRevision: changed.Revision}
	raw, _ = json.Marshal(rollback)
	command("update rollback", raw, config.ErrSnapshotCapacity.Error())
	if err := api.Do(ctx, http.MethodPost, "/v1/config/snapshots/delete",
		control.SnapshotDeleteRequest{ExpectedRevision: changed.Revision, Snapshot: obsolete[1]}, &removed); err != nil || removed != obsolete[1] {
		t.Fatal("HTTP deletion failed", removed, err)
	}
	if err := json.Unmarshal(command("update rollback", raw, ""), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Active == nil || *pending.Active != current || pending.Pending == nil || pending.Pending.Release != previous ||
		pending.Pending.Startup.Snapshot.Revision != changed.Revision {
		t.Fatal("deletion did not restore rollback preparation with the current configuration")
	}
	refuseDelete(pending.Pending.Startup.Snapshot, changed.Revision)
	raw, _ = json.Marshal(control.ReleaseCancelRequest{ExpectedGeneration: pending.Generation})
	command("update cancel", raw, "")
	if saved, err := cfg.Load(); err != nil || saved.Revision != changed.Revision {
		t.Fatal("cleanup or preparation changed the running configuration", err)
	}
	t.Log("SSH/API deletion: stale and selected references refused; same-reference retry succeeded; prepare and rollback recovered at capacity; pending references protected; running release and configuration retained")
}
