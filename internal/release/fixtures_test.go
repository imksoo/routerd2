// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/runtimeabi"
)

var fixtureOnce sync.Once
var fixtureData map[string][]byte
var fixtureError error

const fixtureRelease = "fixture-r1"

func testKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("routerd2 release tests: not a production signing key"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func fixtureBinaries(t *testing.T) map[string][]byte {
	t.Helper()
	fixtureOnce.Do(func() {
		goExecutable := filepath.Join(runtime.GOROOT(), "bin", "go")
		if runtime.GOROOT() == "" {
			// abuild enables -trimpath, which clears the compiled-in GOROOT.
			// Locate the same supplied toolchain through the build environment.
			var err error
			goExecutable, err = exec.LookPath("go")
			if err != nil {
				fixtureError = fmt.Errorf("locate Go fixture toolchain: %w", err)
				return
			}
		}
		dir, err := os.MkdirTemp("", "routerd-release-elf-fixture-")
		if err != nil {
			fixtureError = err
			return
		}
		defer os.RemoveAll(dir)
		if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/imksoo/routerd2\n\ngo 1.24.0\n"), 0600); err != nil {
			fixtureError = err
			return
		}
		fixtureData = map[string][]byte{}
		for _, name := range []string{"routerd", "routerd-modules"} {
			path := filepath.Join(dir, "cmd", name)
			if err = os.MkdirAll(path, 0700); err != nil {
				fixtureError = err
				return
			}
			source := fmt.Sprintf("package main\nimport \"fmt\"\nvar Identity string\nconst RuntimeABI = %q\nfunc main(){fmt.Println(Identity,RuntimeABI)}\n", runtimeabi.CurrentRecord)
			if err = os.WriteFile(filepath.Join(path, "main.go"), []byte(source), 0600); err != nil {
				fixtureError = err
				return
			}
			output := filepath.Join(dir, name)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			command := exec.CommandContext(ctx, goExecutable, "build", "-buildmode=exe", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -X main.Identity="+buildinfo.IdentityPrefix+fixtureRelease+buildinfo.IdentitySuffix, "-o", output, "./cmd/"+name)
			command.Dir = dir
			command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOAMD64=v1", "GOARM64=v8.0", "GOTOOLCHAIN=local", "GOENV=off")
			log, buildErr := command.CombinedOutput()
			cancel()
			if buildErr != nil {
				fixtureError = fmt.Errorf("build ELF fixture: %w\n%s", buildErr, log)
				return
			}
			data, readErr := os.ReadFile(output)
			if readErr != nil {
				fixtureError = readErr
				return
			}
			fixtureData[name] = data
		}
	})
	if fixtureError != nil {
		t.Fatal(fixtureError)
	}
	result := map[string][]byte{}
	for name, data := range fixtureData {
		result[name] = append([]byte(nil), data...)
	}
	return result
}

func binariesFor(t *testing.T, id string) map[string][]byte {
	t.Helper()
	if len(id) != len(fixtureRelease) {
		t.Fatal("test release IDs must have the same length")
	}
	files := fixtureBinaries(t)
	old := []byte(buildinfo.IdentityPrefix + fixtureRelease + buildinfo.IdentitySuffix)
	next := []byte(buildinfo.IdentityPrefix + id + buildinfo.IdentitySuffix)
	for name, b := range files {
		if bytes.Count(b, old) != 1 {
			t.Fatalf("%s fixture identity is not unique", name)
		}
		files[name] = bytes.Replace(b, old, next, 1)
	}
	return files
}

func signedFixture(t *testing.T, id string, sequence uint64, files map[string][]byte) (Manifest, []byte, []byte) {
	t.Helper()
	if files == nil {
		files = binariesFor(t, id)
	}
	key := testKey()
	m := Manifest{Version: ManifestVersion, ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: runtime.GOARCH, KeyID: KeyID(key.Public().(ed25519.PublicKey)), BootABI: 1, WorkerABI: 3, Entrypoint: "routerd", Roles: []RoleBinding{{Role: "logging", Artifact: "routerd-modules"}, {Role: "network", Artifact: "routerd-modules"}, {Role: "control", Artifact: "routerd-modules"}, {Role: "management", Artifact: "routerd-modules"}, {Role: "health", Artifact: "routerd-modules"}, {Role: "dhcp4", Artifact: "routerd-modules"}, {Role: "dhcp6", Artifact: "routerd-modules"}, {Role: "dhcp_server", Artifact: "routerd-modules"}, {Role: "ra", Artifact: "routerd-modules"}, {Role: "ra_client", Artifact: "routerd-modules"}, {Role: "aftr", Artifact: "routerd-modules"}, {Role: "dns", Artifact: "routerd-modules"}, {Role: "vrrp", Artifact: "routerd-modules"}, {Role: "bgp", Artifact: "routerd-modules"}, {Role: "bfd", Artifact: "routerd-modules"}}}
	m.RuntimeABI = runtimeabi.Current()
	for _, name := range []string{"routerd", "routerd-modules"} {
		sum := sha256.Sum256(files[name])
		m.Artifacts = append(m.Artifacts, Artifact{Name: name, Kind: "native", Command: name, Size: int64(len(files[name])), SHA256: hex.EncodeToString(sum[:])})
	}
	raw, sig, err := SignManifest(m, key)
	if err != nil {
		t.Fatal(err)
	}
	return m, raw, sig
}

type archiveEntry struct {
	name   string
	body   []byte
	kind   byte
	link   string
	size   int64
	mode   int64
	format tar.Format
}

func fixtureArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(entry.body))
		if entry.size != 0 {
			size = entry.size
		}
		format := entry.format
		if format == 0 {
			format = tar.FormatUSTAR
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0500
		}
		h := &tar.Header{Name: entry.name, Typeflag: kind, Linkname: entry.link, Mode: mode, Size: size, Format: format, ModTime: time.Unix(0, 0)}
		if kind != tar.TypeReg {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			_, _ = w.Write(entry.body)
		}
	}
	_ = w.Close() // oversized/truncated test entries intentionally fail Close
	return out.Bytes()
}

func entriesFor(t *testing.T, id string, sequence uint64) (Manifest, []archiveEntry) {
	t.Helper()
	files := binariesFor(t, id)
	m, raw, sig := signedFixture(t, id, sequence, files)
	return m, []archiveEntry{{name: "manifest.json", body: raw}, {name: "manifest.sig", body: sig}, {name: "routerd", body: files["routerd"]}, {name: "routerd-modules", body: files["routerd-modules"]}}
}

func bundleFor(t *testing.T, id string, sequence uint64) []byte {
	t.Helper()
	_, entries := entriesFor(t, id, sequence)
	return fixtureArchive(t, entries)
}

func storeOptions(root string) Options {
	key := testKey().Public().(ed25519.PublicKey)
	return Options{Root: root, TrustedKeys: map[string]ed25519.PublicKey{KeyID(key): key}, TargetOS: "linux", TargetArch: runtime.GOARCH}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(storeOptions(filepath.Join(t.TempDir(), "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_ = filepath.WalkDir(s.options.Root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	return s
}

func stageFixture(t *testing.T, s *Store, id string, sequence uint64) Ref {
	t.Helper()
	ref, err := s.Stage(context.Background(), bytes.NewReader(bundleFor(t, id, sequence)))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func mustState(t *testing.T, s *Store) State {
	t.Helper()
	state, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func initializeFixture(t *testing.T, s *Store) Ref {
	t.Helper()
	ref := stageFixture(t, s, "fixture-r1", 1)
	if _, err := s.Initialize(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	return ref
}
