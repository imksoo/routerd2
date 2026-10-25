// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestPowerProbeChecksEachFactoryRoleInode(t *testing.T) {
	files := map[string]os.FileInfo{}
	for _, name := range []string{"routerd", "routerd-modules", "routerd-logging"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(name), 0700); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		files["/test/bin/"+name] = info
	}
	files["/proc/1/exe"] = files["/test/bin/routerd"]
	status := supervisor.Status{}
	for i, role := range []string{"logging", "network", "control", "management", "health"} {
		name, binary := role, "routerd-modules"
		if role == "logging" {
			binary = "routerd-logging"
		}
		if role == "health" {
			name = "service.wan"
		}
		pid := 10 + i
		status.Modules = append(status.Modules, supervisor.ModuleStatus{Name: name, Role: role, Ready: true, PID: pid, ReleaseID: identity})
		files[fmt.Sprintf("/proc/%d/exe", pid)] = files["/test/bin/"+binary]
	}
	stat := func(path string) (os.FileInfo, error) {
		if info := files[path]; info != nil {
			return info, nil
		}
		return nil, os.ErrNotExist
	}
	if err := verifyExecutableCatalog(status, stat); err != nil {
		t.Fatal("fresh command pair and independent logger rejected:", err)
	}
	for _, item := range []struct{ name, path, replacement string }{
		{"wrong supervisor", "/proc/1/exe", "/test/bin/routerd-modules"},
		{"logger uses modules", "/proc/10/exe", "/test/bin/routerd-modules"},
		{"health uses logger", "/proc/14/exe", "/test/bin/routerd-logging"},
		{"missing executable", "/proc/12/exe", "missing"},
	} {
		t.Run(item.name, func(t *testing.T) {
			previous := files[item.path]
			files[item.path] = files[item.replacement]
			defer func() { files[item.path] = previous }()
			if err := verifyExecutableCatalog(status, stat); err == nil {
				t.Fatal("foreign or missing process executable accepted")
			}
		})
	}
	for _, item := range []struct {
		name   string
		mutate func(*supervisor.Status)
	}{
		{"unready", func(s *supervisor.Status) { s.Modules[0].Ready = false }},
		{"foreign release", func(s *supervisor.Status) { s.Modules[0].ReleaseID = "foreign" }},
		{"missing core", func(s *supervisor.Status) { s.Modules = s.Modules[1:] }},
	} {
		t.Run(item.name, func(t *testing.T) {
			changed := status
			changed.Modules = append([]supervisor.ModuleStatus(nil), status.Modules...)
			item.mutate(&changed)
			if err := verifyExecutableCatalog(changed, stat); err == nil {
				t.Fatal("invalid process catalog accepted")
			}
		})
	}
}
