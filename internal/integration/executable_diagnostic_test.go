//go:build linux && integration

package integration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func executableProbeReport(path string, args ...string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err.Error()
	}
	defer devnull.Close()
	run := func(target string, extra []*os.File) error {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		cmd := exec.CommandContext(ctx, target, args...)
		cmd.ExtraFiles = extra
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		return cmd.Run()
	}
	direct := run(path, nil)
	fd4 := run("/proc/self/fd/4", []*os.File{devnull, f})
	parent := run(fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), f.Fd()), nil)
	dir, err := os.MkdirTemp("", "r2-exec-probe-")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	clone := filepath.Join(dir, "image")
	if err := os.WriteFile(clone, data, 0500); err != nil {
		return err.Error()
	}
	cloned, err := os.ReadFile(clone)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("direct=%v fd4=%v parent_fd=%v clone=%v identical_bytes=%t", direct, fd4, parent, run(clone, nil), sha256.Sum256(data) == sha256.Sum256(cloned))
}

// Read-only, scoped diagnostics: only report descriptors matching the fixture's
// executable inodes, plus the supervisor's descriptor 4 used at exec handoff.
func executableFDReport(paths map[string]string, supervisorPID int) string {
	var rows []string
	for name, path := range paths {
		expected, err := os.Stat(path)
		if err != nil {
			continue
		}
		fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
		for _, fd := range fds {
			actual, err := os.Stat(fd)
			if err != nil || !os.SameFile(expected, actual) {
				continue
			}
			parts := strings.Split(fd, "/")
			info, _ := os.ReadFile(filepath.Join("/proc", parts[2], "fdinfo", parts[4]))
			flags := uint64(0)
			for _, line := range strings.Split(string(info), "\n") {
				if strings.HasPrefix(line, "flags:") {
					flags, _ = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "flags:")), 8, 64)
				}
			}
			rows = append(rows, fmt.Sprintf("image=%s pid=%s fd=%s flags=%#o writer=%t", name, parts[2], parts[4], flags, flags&unix.O_ACCMODE != unix.O_RDONLY))
		}
	}
	target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/4", supervisorPID))
	rows = append(rows, fmt.Sprintf("supervisor_fd4=%q", target))
	return strings.Join(rows, "\n")
}

func TestExecutableBusyDiagnosticsIdentifyWriterAndExecFDRecoversAfterClose(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "module")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	run := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/proc/self/fd/4", "-test.run=^$")
		cmd.ExtraFiles = []*os.File{devnull, image}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		return cmd.Run()
	}
	if err := run(); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatal("ETXTBSY not reproduced", err)
	}
	report := executableFDReport(map[string]string{"module": path}, os.Getpid())
	if !strings.Contains(report, "writer=true") {
		t.Fatal("write holder missing from diagnostic", report)
	}
	reportProbe := executableProbeReport(path, "-test.run=^$")
	if !strings.Contains(reportProbe, "clone=<nil> identical_bytes=true") || !strings.Contains(reportProbe, "direct=fork/exec") {
		t.Fatal("writable-inode versus identical-clone control did not distinguish exec", reportProbe)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(executableFDReport(map[string]string{"module": path}, os.Getpid()), "writer=true") {
		t.Fatal("closed writer still reported")
	}
	if err := run(); err != nil {
		t.Fatal("read-only pinned executable still busy after writer close", err)
	}
}
