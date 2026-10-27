// SPDX-License-Identifier: BSD-3-Clause

// Package dns contains the native routerd DNS engine adapted into a callable
// module. It runs no external commands and owns no kernel network configuration.
package dns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"golang.org/x/sys/unix"
)

type Config = dnsconfig.Config

func Validate(c Config) error { return dnsconfig.Validate(c) }
func normalizedRuntime(c dnsresolver.RuntimeConfig, statePath string) dnsresolver.RuntimeConfig {
	return dnsconfig.NormalizeRuntime(c, statePath)
}
func validateRuntime(c dnsresolver.RuntimeConfig) error { return dnsconfig.ValidateRuntime(c) }
func validateRuntimePaths(c dnsresolver.RuntimeConfig, opts options) error {
	return dnsconfig.ValidateRuntimePaths(c, Config{SocketPath: opts.socketPath, StatePath: opts.stateFile, EventPath: opts.eventFile})
}
func validHostname(name string) bool { return dnsconfig.ValidHostname(name) }
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Run consumes complete native DHCP projections independently
// of DNS listeners and upstream pools. Closing the channel retains only the
// already received finite deadlines; it never makes a lease permanent.
func Run(ctx context.Context, cfg Config, updates <-chan dnsconfig.Input, ready func()) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	for _, path := range []string{cfg.SocketPath, cfg.StatePath, cfg.EventPath} {
		if path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
		}
	}
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("DNS state is already owned: %w", err)
	}
	d, err := newDaemon(options{resource: cfg.Runtime.Resource, socketPath: cfg.SocketPath, stateFile: cfg.StatePath, eventFile: cfg.EventPath}, normalizedRuntime(cfg.Runtime, cfg.StatePath))
	if err != nil {
		return err
	}
	d.ready = ready
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if updates != nil {
		select {
		case initial, ok := <-updates:
			if !ok {
				return errors.New("DNS native lease input closed before initial projection")
			}
			if err := d.applyInput(initial); err != nil {
				return err
			}
		case <-runCtx.Done():
			return nil
		}
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() {
			defer close(done)
			for {
				select {
				case <-runCtx.Done():
					return
				case projection, ok := <-updates:
					if !ok {
						return
					}
					if err := d.applyInput(projection); err != nil {
						d.inputRejected.Add(1)
						slog.Warn("DNS lease projection rejected", "resource", cfg.Runtime.Resource, "error", err)
					}
				}
			}
		}()
	}
	return normalizeDaemonExit(ctx, d.Run(runCtx))
}
