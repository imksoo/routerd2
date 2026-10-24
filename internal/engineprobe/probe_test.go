package engineprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperEngineProcess is invoked as a subprocess via the test executable.
// This avoids assumptions about /bin/echo and /bin/sleep, which are often
// BusyBox symlinks on Alpine and are deliberately rejected by engineprobe.
func TestHelperEngineProcess(t *testing.T) {
	var marker int = -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker == -1 {
		return
	}
	if marker+1 >= len(os.Args) {
		os.Exit(20)
	}
	switch os.Args[marker+1] {
	case "echo":
		fmt.Fprintln(os.Stdout, strings.Join(os.Args[marker+2:], " "))
	case "stderr":
		fmt.Fprintln(os.Stderr, "worker stderr")
	case "env":
		fmt.Fprintln(os.Stdout, os.Getenv("ENGINEPROBE_SECRET"))
	case "sleep":
		time.Sleep(500 * time.Millisecond)
	case "exit":
		os.Exit(7)
	default:
		os.Exit(21)
	}
	os.Exit(0)
}

func helperSpec(t *testing.T, mode string, args ...string) Spec {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Name:        "helper",
		Executable:  binary,
		Args:        append([]string{"-test.run=^TestHelperEngineProcess$", "--", mode}, args...),
		Timeout:     3 * time.Second,
		OutputLimit: 128,
	}
}

func TestForegroundProcess(t *testing.T) {
	r, err := Run(context.Background(), helperSpec(t, "echo", "hello"))
	if err != nil || r.ExitCode != 0 || strings.TrimSpace(r.Output) != "hello" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestNoShellInterpretation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-created")
	literal := "$(touch " + file + ")"
	r, err := Run(context.Background(), helperSpec(t, "echo", literal))
	if err != nil || strings.TrimSpace(r.Output) != literal {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("shell interpreted argument or unexpected stat error: %v", err)
	}
}

func TestRejectSymlinkAndBadLimits(t *testing.T) {
	s := helperSpec(t, "echo", "hello")
	link := filepath.Join(t.TempDir(), "helper")
	if err := os.Symlink(s.Executable, link); err != nil {
		t.Fatal(err)
	}
	cases := []Spec{
		{Name: s.Name, Executable: link, Args: s.Args, Timeout: s.Timeout, OutputLimit: s.OutputLimit},
		{Name: s.Name, Executable: s.Executable, Args: s.Args, Timeout: 0, OutputLimit: s.OutputLimit},
		{Name: s.Name, Executable: s.Executable, Args: s.Args, Timeout: s.Timeout, OutputLimit: 0},
		{Name: "bad/name", Executable: s.Executable, Args: s.Args, Timeout: s.Timeout, OutputLimit: s.OutputLimit},
		{Name: s.Name, Executable: "relative-path", Args: s.Args, Timeout: s.Timeout, OutputLimit: s.OutputLimit},
		{Name: s.Name, Executable: s.Executable, Args: []string{"bad\x00arg"}, Timeout: s.Timeout, OutputLimit: s.OutputLimit},
	}
	for _, bad := range cases {
		if _, err := Run(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestTimeout(t *testing.T) {
	s := helperSpec(t, "sleep")
	s.Timeout = 30 * time.Millisecond
	r, err := Run(context.Background(), s)
	if err == nil || !r.TimedOut {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBoundedOutput(t *testing.T) {
	s := helperSpec(t, "echo", strings.Repeat("x", 200))
	s.OutputLimit = 16
	r, err := Run(context.Background(), s)
	if err != nil || !r.Truncated || len(r.Output) != 16 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestExitAndStderr(t *testing.T) {
	r, err := Run(context.Background(), helperSpec(t, "stderr"))
	if err != nil || !strings.Contains(r.Output, "worker stderr") {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = Run(context.Background(), helperSpec(t, "exit"))
	if err == nil || r.ExitCode != 7 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestNoInheritedEnvironment(t *testing.T) {
	t.Setenv("ENGINEPROBE_SECRET", "must-not-leak")
	r, err := Run(context.Background(), helperSpec(t, "env"))
	if err != nil || strings.TrimSpace(r.Output) != "" {
		t.Fatalf("%+v %v", r, err)
	}
}
