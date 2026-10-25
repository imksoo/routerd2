//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/releaseboot"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"golang.org/x/sys/unix"
)

func bootConfigured(path string, b runtimeconfig.Boot) error {
	if b.Release == nil {
		return serve(b)
	}
	applianceRuntime = true
	if err := bootstrapRuntime(context.Background(), b); err != nil {
		return err
	}
	keys, err := release.LoadTrustFile(b.Release.TrustFile)
	if err != nil {
		return err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(bootID))
	store, selection, err := release.OpenForBoot(context.Background(), release.Options{Root: b.Release.Root, TrustedKeys: keys}, id)
	if err != nil {
		return err
	}
	defer store.Close()
	defer selection.Image.Close()
	if selection.State.Startup != nil {
		return errors.New("paired runtime/configuration boot requires the coordinated loader and trial fence; not activated by the current command")
	}
	selected, err := selection.Image.Entrypoint().Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	if !os.SameFile(selected, current) {
		// Execute the verified open inode, preserving PID 1. All store handles
		// are CLOEXEC. The next image re-enters BeginBoot with the same kernel
		// boot ID, so this handoff does not consume another attempt or token.
		image := fmt.Sprintf("/proc/self/fd/%d", selection.Image.Entrypoint().Fd())
		err := unix.Exec(image, []string{"routerd", "boot", "--file", path}, os.Environ())
		if selection.Trial {
			cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
			_, failure := store.FailBoot(cleanup, id, selection.State.Attempt.Token, "selected executable could not start")
			stop()
			err = errors.Join(err, failure)
		}
		return err
	}
	if buildinfo.ReleaseID != selection.Image.Release.ReleaseID {
		return errors.New("executing release identity differs from its verified manifest")
	}
	if !buildinfo.Current().RuntimeABI.Equal(selection.Image.Manifest.RuntimeABI) {
		return errors.New("executing runtime ABI differs from its verified manifest")
	}
	b.Workers = map[string]string{}
	for _, binding := range selection.Image.Manifest.Roles {
		f, err := selection.Image.Executable(binding.Role)
		if err != nil {
			return err
		}
		b.Workers[binding.Role] = fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	}
	authority := &release.Authority{Root: b.Release.Root, TrustFile: b.Release.TrustFile, Digest: selection.Image.Release.Digest}
	if !selection.Trial {
		return serveRuntime(b, nil, authority)
	}
	timeout, stable := b.Release.ReadyTimeoutSeconds, b.Release.StableSeconds
	if timeout == 0 {
		timeout = 120
	}
	if stable == 0 {
		stable = 10
	}
	trial := &releaseboot.Trial{Store: store, BootID: id, Token: selection.State.Attempt.Token, Release: selection.Image.Release, RuntimeDir: b.RuntimeDir, Timeout: time.Duration(timeout) * time.Second, Stable: time.Duration(stable) * time.Second}
	err = serveRuntime(b, trial, authority)
	if err != nil {
		// Early failures before the watcher starts still leave an explicit
		// rollback record. A completed Confirm/FailBoot legitimately fences it.
		failure := trial.Fail(context.Background(), "selected userland failed during startup")
		if !errors.Is(failure, release.ErrStale) {
			err = errors.Join(err, failure)
		}
	}
	return err
}
