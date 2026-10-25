// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package releasecli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/release"
)

type cliFixture struct {
	t           *testing.T
	root, trust string
	key         ed25519.PrivateKey
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	dir := t.TempDir()
	seed := sha256.Sum256([]byte("release CLI tests only, never a deployment key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	public := key.Public().(ed25519.PublicKey)
	raw, err := json.Marshal(map[string]string{release.KeyID(public): base64.StdEncoding.EncodeToString(public)})
	if err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(dir, "trusted.json")
	if err := os.WriteFile(trust, raw, 0644); err != nil {
		t.Fatal(err)
	}
	f := &cliFixture{t: t, root: filepath.Join(dir, "store"), trust: trust, key: key}
	t.Cleanup(func() {
		_ = filepath.WalkDir(f.root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	return f
}

func (f *cliFixture) args(command string, extra ...string) []string {
	args := []string{command, "--root", f.root, "--trust-file", f.trust, "--target-arch", runtime.GOARCH}
	return append(args, extra...)
}

func (f *cliFixture) run(command string, dst any, extra ...string) error {
	f.t.Helper()
	var out, errout bytes.Buffer
	err := Run(context.Background(), f.args(command, extra...), &out, &errout)
	if err != nil {
		if out.Len() != 0 {
			f.t.Fatalf("failed command emitted a success result: %q", out.String())
		}
		return err
	}
	if errout.Len() != 0 {
		f.t.Fatalf("successful command has stderr: %s", errout.String())
	}
	if dst != nil {
		value := reflect.ValueOf(dst).Elem()
		value.Set(reflect.Zero(value.Type()))
		if json.Unmarshal(out.Bytes(), dst) != nil {
			f.t.Fatalf("invalid command JSON: %s", out.String())
		}
	}
	return nil
}

func (f *cliFixture) store() *release.Store {
	f.t.Helper()
	keys, err := release.LoadTrustFile(f.trust)
	if err != nil {
		f.t.Fatal(err)
	}
	store, err := release.Open(release.Options{Root: f.root, TrustedKeys: keys, TargetOS: "linux", TargetArch: runtime.GOARCH})
	if err != nil {
		f.t.Fatal(err)
	}
	return store
}

var binaryOnce sync.Once
var binaries map[string][]byte
var binaryErr error

func cliBinaries(t *testing.T, id string) map[string][]byte {
	t.Helper()
	binaryOnce.Do(func() {
		goExecutable := filepath.Join(runtime.GOROOT(), "bin", "go")
		if runtime.GOROOT() == "" {
			// Trimmed test binaries have no compiled-in toolchain path.
			var err error
			goExecutable, err = exec.LookPath("go")
			if err != nil {
				binaryErr = fmt.Errorf("locate Go fixture toolchain: %w", err)
				return
			}
		}
		dir, err := os.MkdirTemp("", "release-cli-build-")
		if err != nil {
			binaryErr = err
			return
		}
		defer os.RemoveAll(dir)
		if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/imksoo/routerd2\n\ngo 1.24.0\n"), 0600); err != nil {
			binaryErr = err
			return
		}
		binaries = map[string][]byte{}
		for _, name := range []string{"routerd", "routerd-modules"} {
			path := filepath.Join(dir, "cmd", name)
			if err = os.MkdirAll(path, 0700); err != nil {
				binaryErr = err
				return
			}
			if err = os.WriteFile(filepath.Join(path, "main.go"), []byte("package main\nimport \"fmt\"\nvar Identity string\nfunc main(){fmt.Println(Identity)}\n"), 0600); err != nil {
				binaryErr = err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			output := filepath.Join(dir, name)
			command := exec.CommandContext(ctx, goExecutable, "build", "-buildmode=exe", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -X main.Identity="+buildinfo.IdentityPrefix+"fixture-c1"+buildinfo.IdentitySuffix, "-o", output, "./cmd/"+name)
			command.Dir = dir
			command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOAMD64=v1", "GOARM64=v8.0", "GOENV=off", "GOTOOLCHAIN=local")
			log, err := command.CombinedOutput()
			cancel()
			if err != nil {
				binaryErr = fmt.Errorf("build CLI fixture: %w\n%s", err, log)
				return
			}
			b, err := os.ReadFile(output)
			if err != nil {
				binaryErr = err
				return
			}
			binaries[name] = b
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	if len(id) != len("fixture-c1") {
		t.Fatal("invalid fixture identity")
	}
	files := map[string][]byte{}
	for name, data := range binaries {
		files[name] = bytes.Replace(data, []byte(buildinfo.IdentityPrefix+"fixture-c1"+buildinfo.IdentitySuffix), []byte(buildinfo.IdentityPrefix+id+buildinfo.IdentitySuffix), 1)
	}
	return files
}

func (f *cliFixture) bundle(id string, sequence uint64) (string, release.Ref) {
	f.t.Helper()
	dir := f.t.TempDir()
	files := cliBinaries(f.t, id)
	var input []*os.File
	for _, name := range []string{"routerd", "routerd-modules"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0500); err != nil {
			f.t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			f.t.Fatal(err)
		}
		defer file.Close()
		input = append(input, file)
	}
	path := filepath.Join(dir, "release.tar")
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := release.WriteBundle(context.Background(), output, release.BundleOptions{ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 1, Entrypoint: "routerd", Artifacts: []release.Artifact{{Name: "routerd", Kind: "native", Command: "routerd"}, {Name: "routerd-modules", Kind: "native", Command: "routerd-modules"}}, Roles: []release.RoleBinding{{Role: "logging", Artifact: "routerd-modules"}, {Role: "network", Artifact: "routerd-modules"}, {Role: "control", Artifact: "routerd-modules"}, {Role: "management", Artifact: "routerd-modules"}, {Role: "health", Artifact: "routerd-modules"}, {Role: "dhcp4", Artifact: "routerd-modules"}, {Role: "dhcp6pd", Artifact: "routerd-modules"}, {Role: "dhcp_server", Artifact: "routerd-modules"}, {Role: "ra", Artifact: "routerd-modules"}, {Role: "ra_client", Artifact: "routerd-modules"}, {Role: "aftr", Artifact: "routerd-modules"}, {Role: "dns", Artifact: "routerd-modules"}, {Role: "vrrp", Artifact: "routerd-modules"}, {Role: "bgp", Artifact: "routerd-modules"}, {Role: "bfd", Artifact: "routerd-modules"}}}, f.key, map[string]*os.File{"routerd": input[0], "routerd-modules": input[1]})
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return path, release.Ref{Digest: hex.EncodeToString(digest[:]), ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH}
}

func TestCLIStageInitializePrepareCancelRollbackAndPrune(t *testing.T) {
	f := newCLIFixture(t)
	firstPath, first := f.bundle("fixture-c1", 1)
	var ref release.Ref
	if err := f.run("stage", &ref, "--file", firstPath); err != nil || ref != first {
		t.Fatalf("stage: %+v %v", ref, err)
	}
	if err := f.run("verify", &ref, "--digest", first.Digest); err != nil || ref != first {
		t.Fatalf("verify: %+v %v", ref, err)
	}
	var state release.State
	if err := f.run("status", &state); err != nil || state.Generation != 0 {
		t.Fatalf("empty status: %+v %v", state, err)
	}
	if err := f.run("initialize", &state, "--digest", first.Digest); err != nil || state.Generation != 1 || *state.Active != first {
		t.Fatalf("initialize: %+v %v", state, err)
	}
	if err := f.run("initialize", nil, "--digest", first.Digest); err == nil {
		t.Fatal("CLI initialized twice")
	}
	secondPath, second := f.bundle("fixture-c2", 2)
	if err := f.run("stage", &ref, "--file", secondPath); err != nil {
		t.Fatal(err)
	}
	if err := f.run("prepare", nil, "--digest", second.Digest); err == nil || !strings.Contains(err.Error(), "--expected-generation") {
		t.Fatalf("implicit generation accepted: %v", err)
	}
	if err := f.run("prepare", nil, "--digest", second.Digest, "--expected-generation", "0"); !errors.Is(err, release.ErrStale) {
		t.Fatalf("stale CLI prepare: %v", err)
	}
	if err := f.run("prepare", &state, "--digest", second.Digest, "--expected-generation", "1"); err != nil || state.Pending.Release != second {
		t.Fatalf("prepare: %+v %v", state, err)
	}
	if err := f.run("cancel", &state, "--expected-generation", strconv.FormatUint(state.Generation, 10)); err != nil || state.Pending != nil || state.HighWatermark != 2 {
		t.Fatalf("cancel: %+v %v", state, err)
	}
	if err := f.run("prepare", nil, "--digest", second.Digest, "--expected-generation", strconv.FormatUint(state.Generation, 10)); !errors.Is(err, release.ErrDowngrade) {
		t.Fatalf("cancel forgot anti-downgrade policy: %v", err)
	}
	thirdPath, third := f.bundle("fixture-c3", 3)
	if err := f.run("stage", &ref, "--file", thirdPath); err != nil {
		t.Fatal(err)
	}
	if err := f.run("prepare", &state, "--digest", third.Digest, "--expected-generation", strconv.FormatUint(state.Generation, 10), "--max-attempts", "2"); err != nil {
		t.Fatal(err)
	}
	// Only the launcher's library path can record a boot and its health.
	store := f.store()
	selection, err := store.BeginBoot(context.Background(), "cli-test-kernel-boot")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	selection.Image.Close()
	state, err = store.Confirm(context.Background(), "cli-test-kernel-boot", selection.State.Attempt.Token, third.Digest)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.run("rollback", &state, "--expected-generation", strconv.FormatUint(state.Generation, 10), "--reason", "operator selected known-good baseline"); err != nil || *state.Active != first || *state.Previous != third {
		t.Fatalf("rollback: %+v %v", state, err)
	}
	var result release.CleanupResult
	if err := f.run("prune", &result, "--expected-generation", strconv.FormatUint(state.Generation, 10), "--digest", second.Digest); err != nil || len(result.RemovedReleases) != 1 {
		t.Fatalf("prune: %+v %v", result, err)
	}
	if err := f.run("verify", nil, "--digest", second.Digest); err == nil {
		t.Fatal("removed pair still verifies")
	}
}

func TestCLIRequiresExplicitTrustTargetAndGenerationAndHasNoHealthShortcut(t *testing.T) {
	f := newCLIFixture(t)
	for _, args := range [][]string{
		{"status"}, {"stage", "--root", f.root},
		f.args("prepare", "--digest", strings.Repeat("a", 64)),
		f.args("cancel"), f.args("rollback", "--expected-generation", "0"),
		f.args("prune"), f.args("status", "unexpected"),
		f.args("stage", "--file", "x", "--target-arch", "riscv64"),
		f.args("begin-boot"), f.args("confirm"), f.args("fail-boot"), f.args("reboot"),
	} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out, io.Discard); err == nil {
			t.Fatalf("unsafe or incomplete invocation accepted: %v", args)
		}
	}
	if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument errors created a store: %v", err)
	}
	if err := f.run("status", nil); err == nil {
		t.Fatal("status initialized a nonexistent store")
	}
	if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status has creation side effects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, f.args("stage", "--file", "anything"), io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command: %v", err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "--expected-generation") {
		t.Fatalf("help: %v %s", err, out.String())
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("test output pipe is closed") }

func TestCLIOutputFailureReportsCompletedMutation(t *testing.T) {
	f := newCLIFixture(t)
	path, first := f.bundle("fixture-c1", 1)
	if err := f.run("stage", nil, "--file", path); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), f.args("initialize", "--digest", first.Digest), failedWriter{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "operation completed") {
		t.Fatalf("mutation/output boundary: %v", err)
	}
	var state release.State
	if err := f.run("status", &state); err != nil || state.Active == nil || *state.Active != first {
		t.Fatalf("completed initialization was lost: %+v %v", state, err)
	}
}
