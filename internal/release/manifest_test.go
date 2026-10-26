// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

func TestManifestSignatureCanonicalAndTarget(t *testing.T) {
	m, raw, sig := signedFixture(t, "fixture-r1", 1, nil)
	key := testKey().Public().(ed25519.PublicKey)
	keys := map[string]ed25519.PublicKey{KeyID(key): key}
	got, ref, err := VerifyManifest(raw, sig, keys, "linux", runtime.GOARCH, DefaultMaxArtifactBytes)
	if err != nil || got.ReleaseID != m.ReleaseID || !validRef(ref) {
		t.Fatalf("verify: %+v %v", ref, err)
	}
	cases := []struct {
		name     string
		raw, sig []byte
	}{
		{"tamper", bytes.Replace(raw, []byte("fixture-r1"), []byte("fixture-r2"), 1), sig},
		{"unsigned domain", raw, ed25519.Sign(testKey(), raw)},
		{"whitespace", append([]byte(" "), raw...), sig},
		{"duplicate field", append([]byte("{\"version\":1,"), raw[1:]...), sig},
		{"trailing JSON", append(append([]byte(nil), raw...), []byte("{}")...), sig},
		{"unknown field", append(append([]byte(nil), raw[:len(raw)-1]...), []byte(",\"execute\":\"evil\"}")...), sig},
		{"oversized", bytes.Repeat([]byte(" "), MaxManifestBytes+1), sig},
		{"short signature", raw, sig[:63]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := VerifyManifest(tc.raw, tc.sig, keys, "linux", runtime.GOARCH, DefaultMaxArtifactBytes); err == nil {
				t.Fatal("accepted malformed or unsigned manifest")
			}
		})
	}
	if _, _, err := VerifyManifest(raw, sig, nil, "linux", runtime.GOARCH, DefaultMaxArtifactBytes); err == nil {
		t.Fatal("unknown signing key accepted")
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if _, _, err := VerifyManifest(raw, sig, keys, "linux", other, DefaultMaxArtifactBytes); err == nil {
		t.Fatal("wrong architecture accepted")
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Sequence = 0 }, func(m *Manifest) { m.Artifacts = m.Artifacts[:1] },
		func(m *Manifest) { m.Artifacts[1].Name = "routerd" }, func(m *Manifest) { m.Artifacts[0].Size = MaxArtifactBytes + 1 },
		func(m *Manifest) { m.ReleaseID = "../escape" }, func(m *Manifest) { m.KeyID = "publisher" },
	} {
		var candidate Manifest
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatal(err)
		}
		change(&candidate)
		if _, _, err := SignManifest(candidate, testKey()); err == nil {
			t.Fatal("invalid manifest signed")
		}
	}
}

func TestStaticELFIdentityAndOfflineBundle(t *testing.T) {
	files := fixtureBinaries(t)
	for name, b := range files {
		if err := InspectELF(bytes.NewReader(b), int64(len(b)), name, fixtureRelease, "linux", runtime.GOARCH); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	dir := t.TempDir()
	var input []*os.File
	for _, name := range []string{"routerd", "routerd-modules"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0500); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		input = append(input, f)
	}
	var out bytes.Buffer
	manifest, err := WriteBundle(context.Background(), &out, BundleOptions{ReleaseID: fixtureRelease, Sequence: 7, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 3, Entrypoint: "routerd", Artifacts: []Artifact{{Name: "routerd", Kind: "native", Command: "routerd"}, {Name: "routerd-modules", Kind: "native", Command: "routerd-modules"}}, Roles: []RoleBinding{{Role: "logging", Artifact: "routerd-modules"}, {Role: "network", Artifact: "routerd-modules"}, {Role: "control", Artifact: "routerd-modules"}, {Role: "management", Artifact: "routerd-modules"}, {Role: "health", Artifact: "routerd-modules"}, {Role: "dhcp4", Artifact: "routerd-modules"}, {Role: "dhcp6", Artifact: "routerd-modules"}, {Role: "dhcp_server", Artifact: "routerd-modules"}, {Role: "ra", Artifact: "routerd-modules"}, {Role: "ra_client", Artifact: "routerd-modules"}, {Role: "aftr", Artifact: "routerd-modules"}, {Role: "dns", Artifact: "routerd-modules"}, {Role: "vrrp", Artifact: "routerd-modules"}, {Role: "bgp", Artifact: "routerd-modules"}, {Role: "bfd", Artifact: "routerd-modules"}}}, testKey(), map[string]*os.File{"routerd": input[0], "routerd-modules": input[1]})
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	ref, err := store.Stage(context.Background(), bytes.NewReader(out.Bytes()))
	if err != nil || ref.ReleaseID != fixtureRelease || ref.Sequence != manifest.Sequence {
		t.Fatalf("offline publication: %+v %v", ref, err)
	}
	image, err := store.Verify(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	for _, f := range []*os.File{image.Entrypoint(), image.Artifacts["routerd-modules"]} {
		stat, err := f.Stat()
		if err != nil || stat.Mode().Perm() != 0500 {
			t.Fatalf("installed mode: %v %v", stat, err)
		}
	}
	if _, err := store.Stage(context.Background(), bytes.NewReader(out.Bytes())); err != nil {
		t.Fatalf("idempotent stage: %v", err)
	}
}

func TestELFRejectsWrongIdentityAndDynamicOrMalformedPayload(t *testing.T) {
	original := fixtureBinaries(t)["routerd"]
	cases := []struct {
		name         string
		change       func([]byte) []byte
		artifact, id string
	}{
		{"wrong release", nil, "routerd", "fixture-r2"},
		{"wrong command", nil, "routerd-modules", "fixture-r1"},
		{"wrong machine", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[18:20], uint16(elf.EM_386)); return b }, "routerd", "fixture-r1"},
		{"PIE", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[16:18], uint16(elf.ET_DYN)); return b }, "routerd", "fixture-r1"},
		{"huge headers", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[60:62], 65535); return b }, "routerd", "fixture-r1"},
		{"interpreter", func(b []byte) []byte {
			off := binary.LittleEndian.Uint64(b[32:40])
			binary.LittleEndian.PutUint32(b[off:off+4], uint32(elf.PT_INTERP))
			return b
		}, "routerd", "fixture-r1"},
		{"cgo", func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("CGO_ENABLED=0"), []byte("CGO_ENABLED=1")) }, "routerd", "fixture-r1"},
		{"missing identity", func(b []byte) []byte {
			return bytes.Replace(b, []byte("ROUTERD2-RELEASE-V1:fixture-r1:END"), []byte("XXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"), 1)
		}, "routerd", "fixture-r1"},
		{"missing ABI", func(b []byte) []byte {
			return bytes.Replace(b, []byte(runtimeabi.CurrentRecord), bytes.Repeat([]byte("x"), len(runtimeabi.CurrentRecord)), 1)
		}, "routerd", "fixture-r1"},
		{"duplicate ABI", func(b []byte) []byte {
			f, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			section := f.Section(".rodata")
			position := int(section.Offset)
			original := bytes.Index(b, []byte(runtimeabi.CurrentRecord))
			if position+len(runtimeabi.CurrentRecord) > original && position <= original {
				position = original + len(runtimeabi.CurrentRecord) + 32
			}
			if position+len(runtimeabi.CurrentRecord) > int(section.Offset+section.Size) {
				t.Fatal("ABI duplicate fixture exceeds readonly section")
			}
			copy(b[position:], []byte(runtimeabi.CurrentRecord))
			return b
		}, "routerd", "fixture-r1"},
		{"duplicate identity", func(b []byte) []byte {
			f, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			section := f.Section(".rodata")
			copy(b[section.Offset:section.Offset+64], []byte("ROUTERD2-RELEASE-V1:fixture-r1:END"))
			return b
		}, "routerd", "fixture-r1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), original...)
			if tc.change != nil {
				b = tc.change(b)
			}
			if err := InspectELF(bytes.NewReader(b), int64(len(b)), tc.artifact, tc.id, "linux", runtime.GOARCH); err == nil {
				t.Fatal("unsafe ELF accepted")
			}
		})
	}
}

func TestIdentityScanAcrossChunkBoundary(t *testing.T) {
	prefix := bytes.Repeat([]byte{0}, 64<<10-8)
	b := append(prefix, []byte("ROUTERD2-RELEASE-V1:fixture-r1:END")...)
	id, count, err := readIdentity(bytes.NewReader(b))
	if err != nil || id != fixtureRelease || count != 1 {
		t.Fatalf("%s %d %v", id, count, err)
	}
}
