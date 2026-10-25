// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func variableRuntime(t *testing.T) (BundleOptions, map[string]*os.File) {
	t.Helper()
	binaries := fixtureBinaries(t)
	content := map[string][]byte{"supervisor": binaries["routerd"], "coordinator": binaries["routerd-modules"], "kernel": binaries["routerd-modules"], "notices": []byte("test-only source notices\n")}
	files := map[string]*os.File{}
	for name, data := range content {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		files[name] = f
	}
	return BundleOptions{ReleaseID: fixtureRelease, Sequence: 7, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 1, Entrypoint: "supervisor", Artifacts: []Artifact{{Name: "supervisor", Kind: "native", Command: "routerd"}, {Name: "coordinator", Kind: "native", Command: "routerd-modules"}, {Name: "kernel", Kind: "native", Command: "routerd-modules"}, {Name: "notices", Kind: "data"}}, Roles: []RoleBinding{{Role: "control", Artifact: "coordinator"}, {Role: "network", Artifact: "kernel"}, {Role: "health", Artifact: "kernel"}}}, files
}

func TestVariableRuntimeCatalogStagesEveryDeclaredArtifactAndBindsRoles(t *testing.T) {
	opts, files := variableRuntime(t)
	var out bytes.Buffer
	m, err := WriteBundle(context.Background(), &out, opts, testKey(), files)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 4 || m.Entrypoint != "supervisor" {
		t.Fatal(m)
	}
	s := newTestStore(t)
	ref, err := s.Stage(context.Background(), bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.Verify(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if len(image.Artifacts) != 4 || image.Entrypoint() != image.Artifacts["supervisor"] {
		t.Fatal("incomplete pinned runtime")
	}
	for role, name := range map[string]string{"control": "coordinator", "network": "kernel", "health": "kernel"} {
		f, err := image.Executable(role)
		if err != nil || f != image.Artifacts[name] {
			t.Fatal("role bound to wrong component", role, err)
		}
	}
	if _, err := image.Executable("unknown"); err == nil {
		t.Fatal("undeclared role executable accepted")
	}
	for name, f := range image.Artifacts {
		stat, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0500)
		if name == "notices" {
			want = 0400
		}
		if stat.Mode().Perm() != want {
			t.Fatal("artifact type mode mismatch", name, stat.Mode())
		}
	}
	// Content changes in a declared non-executable must also invalidate the
	// whole runtime; signed data cannot be ignored because it is not an ELF.
	path := filepath.Join(s.options.Root, "releases", ref.Digest, "notices")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len("test-only source notices\n"))), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), ref); err == nil {
		t.Fatal("modified signed runtime data accepted")
	}
}

func TestRuntimeCatalogRejectsBrokenBindingsUnboundedSetsAndOldFormat(t *testing.T) {
	opts, files := variableRuntime(t)
	var out bytes.Buffer
	m, err := WriteBundle(context.Background(), &out, opts, testKey(), files)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Version = 1 },
		func(m *Manifest) { m.BootABI++ }, func(m *Manifest) { m.WorkerABI++ },
		func(m *Manifest) { m.Entrypoint = "notices" }, func(m *Manifest) { m.Entrypoint = "missing" },
		func(m *Manifest) { m.Roles[0].Artifact = "missing" }, func(m *Manifest) { m.Roles[0].Artifact = "notices" },
		func(m *Manifest) { m.Roles = append(m.Roles, m.Roles[0]) },
		func(m *Manifest) { m.Artifacts[0].Name = "../escape" }, func(m *Manifest) { m.Artifacts[0].Name = "manifest.json" },
		func(m *Manifest) { m.Artifacts[0].Command = "../routerd" }, func(m *Manifest) { m.Artifacts[0].Kind = "dynamic" },
		func(m *Manifest) { m.Artifacts[3].Command = "routerd" },
		func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) },
		func(m *Manifest) { m.Artifacts = make([]Artifact, MaxArtifacts+1) },
		func(m *Manifest) { m.Roles = make([]RoleBinding, MaxRoles+1) },
	} {
		var bad Manifest
		_ = json.Unmarshal(raw, &bad)
		change(&bad)
		if err := ValidateManifest(bad, MaxArtifactBytes); err == nil {
			t.Fatal("invalid catalog accepted", bad)
		}
	}
	// Old bytes are signed by a trusted key in the former domain. There is no
	// v1 compatibility reader even when the signature itself is authentic.
	legacy := []byte(`{"version":1,"release_id":"fixture-r1","sequence":7,"goos":"linux","goarch":"` + runtime.GOARCH + `","key_id":"` + m.KeyID + `","artifacts":[]}`)
	key := testKey()
	public := key.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(key, append([]byte("routerd2 signed release manifest v1\x00"), legacy...))
	if _, _, err := VerifyManifest(legacy, sig, map[string]ed25519.PublicKey{KeyID(public): public}, "linux", runtime.GOARCH, MaxArtifactBytes); err == nil {
		t.Fatal("old two-ELF format accepted")
	}
}
