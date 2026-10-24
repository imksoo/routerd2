// Package engineprobe provides an isolated pre-adoption test of external foreground
// daemons. It must not be called from routerd PID 1: that process has its own
// central child reaper. No shell or service manager is involved.
package engineprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Spec struct {
	Name        string
	Executable  string
	Args        []string
	Timeout     time.Duration
	OutputLimit int
}
type Result struct {
	Name      string
	ExitCode  int
	TimedOut  bool
	Output    string
	Truncated bool
	Duration  time.Duration
}
type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - len(b.data)
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func Run(parent context.Context, s Spec) (Result, error) {
	result := Result{Name: s.Name, ExitCode: -1}
	if s.Name == "" || strings.ContainsAny(s.Name, "/\\\x00") {
		return result, errors.New("invalid engine name")
	}
	if !filepath.IsAbs(s.Executable) || filepath.Clean(s.Executable) != s.Executable {
		return result, errors.New("executable must be a clean absolute path")
	}
	info, err := os.Lstat(s.Executable)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return result, errors.New("engine must be an executable regular file")
	}
	if s.Timeout <= 0 || s.Timeout > 5*time.Minute {
		return result, errors.New("timeout must be in (0,5m]")
	}
	if s.OutputLimit <= 0 || s.OutputLimit > 1<<20 {
		return result, errors.New("output limit must be in (0,1MiB]")
	}
	for _, a := range s.Args {
		if strings.ContainsRune(a, 0) {
			return result, errors.New("NUL argument")
		}
	}
	ctx, cancel := context.WithTimeout(parent, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Executable, s.Args...)
	cmd.Env = []string{"PATH=/nonexistent", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = 2 * time.Second
	buf := &boundedBuffer{limit: s.OutputLimit}
	cmd.Stdout = buf
	cmd.Stderr = buf
	started := time.Now()
	err = cmd.Run()
	result.Duration = time.Since(started)
	result.Output = string(bytes.ToValidUTF8(buf.data, []byte("?")))
	result.Truncated = buf.truncated
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	if result.TimedOut {
		return result, fmt.Errorf("engine %s exceeded timeout: %w", s.Name, ctx.Err())
	}
	if err != nil {
		return result, fmt.Errorf("engine %s: %w", s.Name, err)
	}
	return result, nil
}
