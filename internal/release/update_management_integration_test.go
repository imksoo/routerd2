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
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	root, err := release.Open(release.Options{Root: releaseDir, TrustedKeys: map[string]ed25519.PublicKey{hex.EncodeToString(digest[:]): pub}})
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
		files[name] = f
		artifacts = append(artifacts, release.Artifact{Name: name, Kind: "native", Command: name})
	}
	var roles []release.RoleBinding
	for role, artifact := range runtimeconfig.NativeWorkers("routerd-modules") {
		if role == "logging" {
			artifact = "routerd-logging"
		}
		roles = append(roles, release.RoleBinding{Role: role, Artifact: artifact})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	stage := func(sequence uint64) release.Ref {
		t.Helper()
		file, err := os.CreateTemp(t.TempDir(), "runtime-*.tar")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		_, err = release.WriteBundle(ctx, file, release.BundleOptions{ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 5, Entrypoint: "routerd", Artifacts: artifacts, Roles: roles}, key, files)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		ref, err := root.Stage(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	previous, current := stage(1), stage(2)
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
	raw, _ = json.Marshal(request)
	pending := command("update rollback", raw, false)
	if pending.Active == nil || *pending.Active != current || pending.Pending == nil || pending.Pending.Release != previous || pending.Operation != "prepare-rollback" || pending.HighWatermark != current.Sequence {
		t.Fatal("rollback did not queue the actual previous runtime while preserving current selection")
	}
	snapshot := pending.Pending.Startup.Snapshot
	if snapshot.StoreID != committed.StoreID || snapshot.Revision != committed.Revision {
		t.Fatal("rollback did not preserve current configuration")
	}
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
	t.Log("SSH rollback: verified previous product compiler accepted current configuration; stale revision/generation and duplicate rejected; active runtime/configuration unchanged; cancellation preserved watermarks")
}
