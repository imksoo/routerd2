// SPDX-License-Identifier: BSD-3-Clause
//go:build linux && integration

package release

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// Run only after an explicit mount-namespace unshare. Reproduces the failed
// fixture geometry on a real bounded tmpfs before another QEMU attempt.
func TestRuntimeCatalogBoundedStagingScratch(t *testing.T) {
	parent := os.Getenv("ROUTERD2_RELEASE_CAPACITY_PARENT_NS")
	current, err := os.Readlink("/proc/self/ns/mnt")
	if parent == "" || err != nil || parent == current || os.Geteuid() != 0 {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("explicit isolated mount namespace is required")
		}
		t.Skip("explicit isolated mount namespace is required")
	}
	dir, id := os.Getenv("ROUTERD2_RELEASE_BUILT_DIR"), os.Getenv("ROUTERD2_RELEASE_BUILT_ID")
	if dir == "" || id == "" {
		t.Fatal("fresh build directory and identity required")
	}
	inputs := map[string]*os.File{}
	for _, name := range []string{"routerd", "routerd-modules", "routerd-logging"} {
		f, err := os.Open(filepath.Join(dir, "linux-"+runtime.GOARCH, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		inputs[name] = f
	}
	for _, compact := range []bool{false, true} {
		name := "duplicated-multicall"
		if compact {
			name = "independent-logging"
		}
		t.Run(name, func(t *testing.T) {
			quota := t.TempDir()
			if err := unix.Mount("tmpfs", quota, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=256m,mode=0700"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := unix.Unmount(quota, 0); err != nil {
					t.Error(err)
				}
			}()
			key := testKey()
			public := key.Public().(ed25519.PublicKey)
			store, err := Open(Options{Root: filepath.Join(quota, "store"), TrustedKeys: map[string]ed25519.PublicKey{KeyID(public): public}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			opts := BundleOptions{ReleaseID: id, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 1, Entrypoint: "supervisor", Artifacts: []Artifact{{Name: "supervisor", Kind: "native", Command: "routerd"}, {Name: "services", Kind: "native", Command: "routerd-modules"}}}
			files := map[string]*os.File{"supervisor": inputs["routerd"], "services": inputs["routerd-modules"]}
			if compact {
				opts.Artifacts = append(opts.Artifacts, Artifact{Name: "logger", Kind: "native", Command: "routerd-logging"})
				files["logger"] = inputs["routerd-logging"]
				opts.Roles = []RoleBinding{{Role: "control", Artifact: "services"}, {Role: "network", Artifact: "services"}, {Role: "logging", Artifact: "logger"}}
			} else {
				for _, name := range []string{"coordinator", "kernel"} {
					opts.Artifacts = append(opts.Artifacts, Artifact{Name: name, Kind: "native", Command: "routerd-modules"})
					files[name] = inputs["routerd-modules"]
				}
				opts.Roles = []RoleBinding{{Role: "control", Artifact: "coordinator"}, {Role: "network", Artifact: "kernel"}, {Role: "logging", Artifact: "services"}}
			}
			var runErr error
			for sequence := uint64(1); sequence <= 2; sequence++ {
				opts.Sequence = sequence
				path := filepath.Join(quota, "incoming.tar")
				out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = WriteBundle(context.Background(), out, opts, key, files)
				err = errors.Join(err, out.Close())
				if err == nil {
					_, err = store.StageFile(context.Background(), path)
				}
				_ = os.Remove(path)
				if err != nil {
					runErr = err
					break
				}
			}
			if compact && runErr != nil {
				t.Fatal("independent native runtime exceeds unchanged scratch bound", runErr)
			}
			if !compact && !errors.Is(runErr, unix.ENOSPC) {
				t.Fatal("failed QEMU duplicate geometry not reproduced", runErr)
			}
			t.Logf("bounded256MiB actual tmpfs compact=%t result=%v", compact, runErr)
		})
	}
}
