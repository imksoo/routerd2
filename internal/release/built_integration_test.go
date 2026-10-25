// SPDX-License-Identifier: BSD-3-Clause
//go:build linux && integration

package release

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// This gate uses the actual two command builds for both supported targets.
// Unlike the small parser fixtures, these include the complete runtime modules.
// Neither architecture's incoming binaries are executed.
func TestBuiltRuntimeCatalog(t *testing.T) {
	dir, id := os.Getenv("ROUTERD2_RELEASE_BUILT_DIR"), os.Getenv("ROUTERD2_RELEASE_BUILT_ID")
	if dir == "" || id == "" {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("ROUTERD2_RELEASE_BUILT_DIR and ROUTERD2_RELEASE_BUILT_ID are required")
		}
		t.Skip("set the built command directory and expected immutable release ID")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			ctx := context.Background()
			var input []*os.File
			for _, name := range []string{"routerd", "routerd-modules", "routerd-logging"} {
				f, err := os.Open(filepath.Join(dir, "linux-"+arch, name))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				stat, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if err = InspectELF(f, stat.Size(), name, id, "linux", arch); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				input = append(input, f)
			}
			bundle, err := os.CreateTemp(t.TempDir(), "release-*.tar")
			if err != nil {
				t.Fatal(err)
			}
			path := bundle.Name()
			manifest, err := WriteBundle(ctx, bundle, BundleOptions{ReleaseID: id, Sequence: 42, GOOS: "linux", GOARCH: arch, BootABI: 1, WorkerABI: 1, Entrypoint: "routerd", Artifacts: []Artifact{{Name: "routerd", Kind: "native", Command: "routerd"}, {Name: "routerd-modules", Kind: "native", Command: "routerd-modules"}, {Name: "routerd-logging", Kind: "native", Command: "routerd-logging"}}, Roles: []RoleBinding{{Role: "logging", Artifact: "routerd-logging"}, {Role: "network", Artifact: "routerd-modules"}, {Role: "control", Artifact: "routerd-modules"}, {Role: "management", Artifact: "routerd-modules"}, {Role: "health", Artifact: "routerd-modules"}, {Role: "dhcp4", Artifact: "routerd-modules"}, {Role: "dhcp6pd", Artifact: "routerd-modules"}, {Role: "dhcp_server", Artifact: "routerd-modules"}, {Role: "ra", Artifact: "routerd-modules"}, {Role: "ra_client", Artifact: "routerd-modules"}, {Role: "aftr", Artifact: "routerd-modules"}, {Role: "dns", Artifact: "routerd-modules"}, {Role: "vrrp", Artifact: "routerd-modules"}, {Role: "bgp", Artifact: "routerd-modules"}, {Role: "bfd", Artifact: "routerd-modules"}}}, testKey(), map[string]*os.File{"routerd": input[0], "routerd-modules": input[1], "routerd-logging": input[2]})
			if err != nil {
				bundle.Close()
				t.Fatal(err)
			}
			if err = bundle.Sync(); err != nil {
				bundle.Close()
				t.Fatal(err)
			}
			if err = bundle.Close(); err != nil {
				t.Fatal(err)
			}
			store := newTestStore(t)
			store.options.TargetArch = arch
			ref, err := store.StageFile(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if ref.ReleaseID != id || ref.Sequence != 42 || ref.GOARCH != arch {
				t.Fatalf("staged actual bundle identity: %+v", ref)
			}
			resolved, err := store.Lookup(ctx, ref.Digest)
			if err != nil || resolved != ref {
				t.Fatalf("digest lookup: %+v %v", resolved, err)
			}
			if _, err = store.Initialize(ctx, ref); err != nil {
				t.Fatal(err)
			}
			selection, err := store.BeginBoot(ctx, "built-pair-fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer selection.Image.Close()
			if selection.Trial || selection.Image.Release != ref {
				t.Fatal("baseline boot did not return its pinned pair")
			}
			t.Logf("%s signed and staged: routerd=%d bytes, modules=%d bytes", arch, manifest.Artifacts[0].Size, manifest.Artifacts[1].Size)
		})
	}
}
