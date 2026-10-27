// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// Package conntracksync owns the conntrackd process used by one VRRP instance.
package conntracksync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/engineimage"
	"github.com/imksoo/routerd2/internal/protocol/conntrackconfig"
	"golang.org/x/sys/unix"
)

// RunEngine is the exec boundary. The daemon owns only conntrack replication;
// ordinary addresses, routes and firewall rules remain with the network writer.
func RunEngine() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := os.NewFile(3, "conntrackd-config")
	var cfg conntrackconfig.Config
	err := json.NewDecoder(f).Decode(&cfg)
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := conntrackconfig.Validate(cfg); err != nil {
		return err
	}
	if err := unix.Unshare(unix.CLONE_NEWNS | unix.CLONE_FS); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
		return err
	}
	root, err := os.MkdirTemp("/tmp", "conntrack-")
	if err != nil {
		return err
	}
	if err := unix.Chroot(root); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	image := os.NewFile(4, "conntrackd-image")
	if err := engineimage.Extract(image); err != nil {
		image.Close()
		return err
	}
	if err := image.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll("/run", 0700); err != nil {
		return err
	}
	if err := os.WriteFile("/conntrackd.conf", conntrackconfig.Render(cfg), 0600); err != nil {
		return err
	}
	return syscall.Exec("/usr/sbin/conntrackd", []string{"conntrackd", "-C", "/conntrackd.conf"}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"})
}

// RunCommand enters the already running engine's pinned root through fd 3.
// There is no command lookup or configuration fallback in the host filesystem.
func RunCommand(action string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch action {
	case "-c", "-R", "-B", "-t", "-n":
	default:
		return errors.New("unsupported conntrackd operation")
	}
	if err := unix.Fchdir(3); err != nil {
		return err
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Close(3)
	return syscall.Exec("/usr/sbin/conntrackd", []string{"conntrackd", "-C", "/conntrackd.conf", action}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"})
}

type Process struct {
	ctx     context.Context
	stop    context.CancelFunc
	done    chan struct{}
	err     error // written before done is closed
	root    *os.File
	primary bool
}

func Start(ctx context.Context, cfg conntrackconfig.Config, image *os.File) (*Process, error) {
	if err := conntrackconfig.Validate(cfg); err != nil {
		return nil, err
	}
	if _, err := image.Stat(); err != nil {
		return nil, fmt.Errorf("VRRP conntrack sync requires its bundled engine: %w", err)
	}
	fd, err := unix.MemfdCreate("conntrackd-config", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "conntrackd-config")
	defer f.Close()
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// The VRRP worker holds its OS thread through Close for PDEATHSIG ownership.
	// Keep the engine available during VRRP's bounded withdrawal after ctx
	// cancellation. Close owns its final termination after withdrawal returns.
	life, stop := context.WithCancel(context.Background())
	worker, failed := context.WithCancel(ctx)
	cmd := exec.CommandContext(life, "/proc/self/exe", "conntrackd-engine")
	cmd.ExtraFiles = []*os.File{f, image}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		stop()
		failed()
		return nil, err
	}
	p := &Process{ctx: worker, stop: stop, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
		failed()
	}()
	base := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/root"
	startup, cancel := context.WithTimeout(worker, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(startup, "unix", base+"/run/conntrackd.ctl")
		if err == nil {
			conn.Close()
			p.root, err = os.Open(base)
			if err == nil {
				return p, nil
			}
			p.Close()
			return nil, err
		}
		select {
		case <-startup.Done():
			exit := p.Err()
			p.Close()
			return nil, errors.Join(errors.New("conntrackd did not open its control socket"), exit, startup.Err())
		case <-tick.C:
		}
	}
}

func (p *Process) Context() context.Context { return p.ctx }

func (p *Process) Err() error {
	select {
	case <-p.done:
		return errors.Join(errors.New("conntrackd exited"), p.err)
	default:
		return nil
	}
}

func (p *Process) Close() {
	p.stop()
	<-p.done
	if p.root != nil {
		p.root.Close()
	}
}

func (p *Process) command(ctx context.Context, action string) ([]byte, error) {
	if err := p.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/proc/self/exe", "conntrackd-command", action)
	cmd.ExtraFiles = []*os.File{p.root}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = time.Second
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("conntrackd %s: %w: %s", action, err, diagnostic.Bytes())
	}
	return output, nil
}

func (p *Process) Promote(ctx context.Context) error {
	if p.primary {
		return p.Err()
	}
	for _, action := range []string{"-c", "-R", "-B"} {
		output, err := p.command(ctx, action)
		if err != nil {
			return err
		}
		if action == "-c" {
			if err := committed(output); err != nil {
				return err
			}
		}
	}
	p.primary = true
	return nil
}

func (p *Process) Demote(ctx context.Context) error {
	if !p.primary {
		return p.Err()
	}
	for _, action := range []string{"-t", "-n"} {
		if _, err := p.command(ctx, action); err != nil {
			return err
		}
	}
	p.primary = false
	return nil
}

// The bundled patch reports both completed kernel commits before closing the
// upstream control socket. CLI exit0 alone does not report partial failures.
func committed(output []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	for _, cache := range []string{"conntrack", "expectation"} {
		var result struct {
			Cache  string `json:"cache"`
			Failed uint64 `json:"failed"`
		}
		if err := decoder.Decode(&result); err != nil {
			return fmt.Errorf("decode conntrackd %s commit result: %w", cache, err)
		}
		if result.Cache != cache {
			return fmt.Errorf("conntrackd commit returned %q, expected %s", result.Cache, cache)
		}
		if result.Failed != 0 {
			return fmt.Errorf("conntrackd failed to commit %d %s entries", result.Failed, cache)
		}
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected data after conntrackd commit result")
	}
	return nil
}
