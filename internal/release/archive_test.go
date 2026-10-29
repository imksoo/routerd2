// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

func TestArchiveRejectsUnsafeEntriesBeforePublishing(t *testing.T) {
	tests := []struct {
		name   string
		change func([]archiveEntry) []archiveEntry
		raw    func([]byte) []byte
	}{
		{"absolute path", func(e []archiveEntry) []archiveEntry { e[2].name = "/routerd"; return e }, nil},
		{"traversal", func(e []archiveEntry) []archiveEntry { e[2].name = "../routerd"; return e }, nil},
		{"alternate path", func(e []archiveEntry) []archiveEntry { e[2].name = "./routerd"; return e }, nil},
		{"duplicate executable", func(e []archiveEntry) []archiveEntry { e[3].name = "routerd"; return e }, nil},
		{"symlink", func(e []archiveEntry) []archiveEntry {
			e[2].kind = tar.TypeSymlink
			e[2].link = "/tmp/escape"
			return e
		}, nil},
		{"hard link", func(e []archiveEntry) []archiveEntry {
			e[2].kind = tar.TypeLink
			e[2].link = "routerd-modules"
			return e
		}, nil},
		{"directory", func(e []archiveEntry) []archiveEntry { e[2].kind = tar.TypeDir; return e }, nil},
		{"device", func(e []archiveEntry) []archiveEntry { e[2].kind = tar.TypeChar; return e }, nil},
		{"GNU header", func(e []archiveEntry) []archiveEntry { e[2].format = tar.FormatGNU; return e }, nil},
		{"PAX name", func(e []archiveEntry) []archiveEntry {
			e[2].name = strings.Repeat("long", 40)
			e[2].format = tar.FormatPAX
			return e
		}, nil},
		{"wrong order", func(e []archiveEntry) []archiveEntry { e[0], e[1] = e[1], e[0]; return e }, nil},
		{"long signature", func(e []archiveEntry) []archiveEntry { e[1].body = append(e[1].body, 0); return e }, nil},
		{"bad signature", func(e []archiveEntry) []archiveEntry { e[1].body[0] ^= 1; return e }, nil},
		{"corrupt ELF body", func(e []archiveEntry) []archiveEntry { e[2].body[len(e[2].body)/2] ^= 1; return e }, nil},
		{"extra file", func(e []archiveEntry) []archiveEntry {
			return append(e, archiveEntry{name: "extra", body: []byte("untrusted")})
		}, nil},
		{"oversized body header", func(e []archiveEntry) []archiveEntry {
			e = e[:3]
			e[2].size = MaxArtifactBytes
			e[2].body = nil
			return e
		}, nil},
		{"invalid checksum", nil, func(b []byte) []byte { b[30] ^= 1; return b }},
		{"nonzero padding", nil, func(b []byte) []byte { b[512+len(mustJSONFixture(t))] = 1; return b }},
		{"truncated payload", nil, func(b []byte) []byte { return b[:len(b)/2] }},
		{"missing trailer", nil, func(b []byte) []byte { return b[:len(b)-1024] }},
		{"concatenated garbage", nil, func(b []byte) []byte { return append(b, 1) }},
		{"excessive padding", nil, func(b []byte) []byte { return append(b, make([]byte, MaxTrailerBytes+1)...) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			_, entries := entriesFor(t, "fixture-r1", 1)
			if tc.change != nil {
				entries = tc.change(entries)
			}
			archive := fixtureArchive(t, entries)
			if tc.raw != nil {
				archive = tc.raw(archive)
			}
			if _, err := store.Stage(context.Background(), bytes.NewReader(archive)); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			names, err := directoryNames(store.releases, 100)
			if err != nil || len(names) != 0 {
				t.Fatalf("failed archive published files: %v %v", names, err)
			}
			state := mustState(t, store)
			if state.Generation != 0 || state.Active != nil || state.Pending != nil {
				t.Fatalf("stage changed boot selection: %+v", state)
			}
		})
	}
}

func mustJSONFixture(t *testing.T) []byte {
	_, raw, _ := signedFixture(t, "fixture-r1", 1, nil)
	return raw
}

func TestSignedButWrongELFDoesNotPublish(t *testing.T) {
	for _, kind := range []string{"swapped", "release", "dynamic"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t)
			files := binariesFor(t, "fixture-r1")
			switch kind {
			case "swapped":
				files["routerd"], files["routerd-modules"] = files["routerd-modules"], files["routerd"]
			case "release":
				files["routerd-modules"] = binariesFor(t, "fixture-r2")["routerd-modules"]
			case "dynamic":
				files["routerd"][16] = 3
			}
			_, raw, sig := signedFixture(t, "fixture-r1", 1, files)
			entries := []archiveEntry{{name: "manifest.json", body: raw}, {name: "manifest.sig", body: sig}, {name: "routerd", body: files["routerd"]}, {name: "routerd-modules", body: files["routerd-modules"]}}
			if _, err := store.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries))); err == nil {
				t.Fatal("incorrect signed pair accepted")
			}
		})
	}
}

func TestSignatureIsVerifiedBeforeExecutablePayloadRead(t *testing.T) {
	store := newTestStore(t)
	_, entries := entriesFor(t, "fixture-r1", 1)
	entries[1].body[1] ^= 1
	prefix := fixtureArchive(t, entries[:2])
	// Exclude the tar terminator; any attempt to read an artifact fails loudly.
	reader := io.MultiReader(bytes.NewReader(prefix[:len(prefix)-1024]), errorReader{})
	if _, err := store.Stage(context.Background(), reader); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("signature not checked first: %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("executable payload must not be read")
}

func TestStageUsesOwnedPermissionsAndRejectsNonregularArchive(t *testing.T) {
	store := newTestStore(t)
	_, entries := entriesFor(t, "fixture-r1", 1)
	for i := range entries {
		entries[i].mode = 07777
	}
	ref, err := store.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"routerd", "routerd-modules"} {
		info, err := os.Stat(filepath.Join(store.options.Root, "releases", ref.Digest, name))
		if err != nil || info.Mode().Perm() != 0500 || info.Mode()&os.ModeSetuid != 0 {
			t.Fatalf("tar permissions escaped: %v %v", info, err)
		}
	}
	archive := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(archive, bundleFor(t, "fixture-r2", 2), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StageFile(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(archive, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StageFile(context.Background(), link); err == nil {
		t.Fatal("symlink archive accepted")
	}
	if _, err := store.StageFile(context.Background(), filepath.Dir(archive)); err == nil {
		t.Fatal("directory archive accepted")
	}
}

func TestSignerCannotSetUnboundedManifestAndLocalTrustIsCloned(t *testing.T) {
	store := newTestStore(t)
	m, entries := entriesFor(t, "fixture-r1", 1)
	m.Artifacts[0].Size = store.options.MaxArtifactBytes + 1
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].body = raw
	entries[1].body = ed25519.Sign(testKey(), append([]byte(signatureDomain), raw...))
	if _, err := store.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries))); err == nil {
		t.Fatal("publisher bypassed local artifact limit")
	}
	options := storeOptions(filepath.Join(t.TempDir(), "store"))
	copyStore, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	for _, key := range options.TrustedKeys {
		key[0] ^= 1
	}
	if _, err := copyStore.Stage(context.Background(), bytes.NewReader(bundleFor(t, "fixture-r1", 1))); err != nil {
		t.Fatalf("caller mutated store's key map: %v", err)
	}
	// Allow t.TempDir's cleanup of the immutable installed directories.
	for _, parent := range []string{"generations", "releases"} {
		dirEntries, _ := os.ReadDir(filepath.Join(options.Root, parent))
		for _, entry := range dirEntries {
			_ = os.Chmod(filepath.Join(options.Root, parent, entry.Name()), 0700)
		}
	}
}

type gateReader struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (g *gateReader) Read([]byte) (int, error) {
	g.once.Do(func() { close(g.started) })
	<-g.release
	return 0, io.EOF
}

func TestStagingAllowsStatusButExcludesSelectionWritesAndClose(t *testing.T) {
	store := newTestStore(t)
	other, err := Open(store.options)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	reader := &gateReader{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := store.Stage(context.Background(), reader); done <- err }()
	<-reader.started
	for _, readerStore := range []*Store{store, other} {
		state, err := readerStore.Status(context.Background())
		if err != nil || state.Generation != 0 {
			t.Fatalf("selection unavailable during archive receive: %+v, %v", state, err)
		}
		if err := readerStore.WithState(context.Background(), func(State) error { return nil }); !errors.Is(err, ErrBusy) {
			t.Fatalf("selection write was not excluded: %v", err)
		}
	}
	if err := store.Close(); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent close: %v", err)
	}
	close(reader.release)
	if err := <-done; err == nil {
		t.Fatal("empty archive accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Status(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}

func TestBundleHeaderSizeDoesNotAllocateDeclaredArtifact(t *testing.T) {
	store := newTestStore(t)
	_, entries := entriesFor(t, "fixture-r1", 1)
	entries = entries[:3]
	entries[2].size = 8 << 30
	entries[2].body = nil
	// USTAR's octal size has an 8 GiB limit; use the largest representable size.
	entries[2].size--
	if _, err := store.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries))); err == nil {
		t.Fatal("unbounded artifact header accepted")
	}
}

func FuzzManifestVerification(f *testing.F) {
	m := Manifest{Version: ManifestVersion, ReleaseID: "r1", Sequence: 1, GOOS: "linux", GOARCH: "amd64", KeyID: KeyID(testKey().Public().(ed25519.PublicKey)), BootABI: 1, WorkerABI: 5, Entrypoint: "routerd", Roles: []RoleBinding{{Role: "control", Artifact: "routerd-modules"}}, Artifacts: []Artifact{{Name: "routerd", Kind: "native", Command: "routerd", Size: 64, SHA256: strings.Repeat("a", 64)}, {Name: "routerd-modules", Kind: "native", Command: "routerd-modules", Size: 64, SHA256: strings.Repeat("b", 64)}}}
	m.RuntimeABI = runtimeabi.Current()
	raw, sig, _ := SignManifest(m, testKey())
	f.Add(raw, sig)
	f.Add([]byte("{}"), []byte("invalid"))
	keys := map[string]ed25519.PublicKey{m.KeyID: testKey().Public().(ed25519.PublicKey)}
	f.Fuzz(func(t *testing.T, raw, signature []byte) {
		if len(raw) > MaxManifestBytes+1 || len(signature) > 128 {
			return
		}
		_, _, _ = VerifyManifest(raw, signature, keys, "linux", "amd64", DefaultMaxArtifactBytes)
	})
}

func ExampleManifest() {
	fmt.Println("manifest.json + manifest.sig + routerd + routerd-modules")
	// Output: manifest.json + manifest.sig + routerd + routerd-modules
}
