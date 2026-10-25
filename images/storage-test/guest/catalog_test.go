//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestDiskProbeUsesVerifiedEntrypointAndPerRoleInodes(t *testing.T) {
	image := &release.Image{Release: release.Ref{ReleaseID: "selected"}, Manifest: release.Manifest{Entrypoint: "supervisor", Roles: []release.RoleBinding{{Role: "control", Artifact: "services"}, {Role: "network", Artifact: "services"}, {Role: "management", Artifact: "services"}, {Role: "logging", Artifact: "logger"}}}, Artifacts: map[string]*os.File{}}
	for _, name := range []string{"supervisor", "services", "logger"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(name), 0700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		image.Artifacts[name] = f
	}
	defer image.Close()
	proc := map[int]os.FileInfo{}
	proc[1], _ = image.Entrypoint().Stat()
	status := supervisor.Status{}
	for i, role := range []string{"control", "network", "management", "logging"} {
		pid := 10 + i
		file, err := image.Executable(role)
		if err != nil {
			t.Fatal(err)
		}
		proc[pid], _ = file.Stat()
		status.Modules = append(status.Modules, supervisor.ModuleStatus{Name: role, Role: role, Ready: true, PID: pid, Required: role != "logging", ReleaseID: "selected"})
	}
	stat := func(pid int) (os.FileInfo, error) { return proc[pid], nil }
	if _, err := verifyExecutableCatalog(status, image.Release, image, stat); err != nil {
		t.Fatal("new signed names rejected", err)
	}
	proc[13], _ = image.Artifacts["services"].Stat()
	if _, err := verifyExecutableCatalog(status, image.Release, image, stat); err == nil {
		t.Fatal("logging worker using another role inode accepted")
	}
}
