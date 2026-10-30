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
	"sync"
	"time"

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
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Run consumes complete learned DNS and DHCP lease inputs independently
// of DNS listeners and upstream pools. Closing the channel retains only the
// already received finite deadlines; it never makes a lease permanent.
func Run(ctx context.Context, cfg Config, updates <-chan dnsconfig.Input, report func(context.Context, dnsconfig.Snapshot) error, ready func()) error {
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
	d, err := newDaemon(options{resource: cfg.Runtime.Resource, socketPath: cfg.SocketPath, stateFile: cfg.StatePath, eventFile: cfg.EventPath, listenInterface: cfg.ListenInterface}, normalizedRuntime(cfg.Runtime, cfg.StatePath))
	if err != nil {
		return err
	}
	d.ready = ready
	d.report = report
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if updates != nil {
		select {
		case initial, ok := <-updates:
			if !ok {
				return errors.New("DNS input closed before its initial snapshot")
			}
			if err := d.applyInput(initial); err != nil {
				return err
			}
		case <-runCtx.Done():
			return nil
		}
	}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	if updates != nil {
		workers.Go(func() {
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
						slog.Warn("DNS input rejected", "resource", cfg.Runtime.Resource, "error", err)
					}
				}
			}
		})
	}
	// Reporting to control can wait on another process. Learned inputs must
	// advance independently, before their originating protocol deadlines.
	workers.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if len(d.listenSources) > 0 {
				if err := d.refreshListeners(); err != nil {
					slog.Warn("DNS listener observation failed", "resource", cfg.Runtime.Resource, "error", err)
				}
			}
			if err := d.publishListeners(runCtx); err != nil && runCtx.Err() == nil {
				slog.Warn("DNS listener observation delivery failed", "resource", cfg.Runtime.Resource, "error", err)
			}
		}
	})
	if cfg.QueryRetention != nil {
		workers.Go(func() {
			select {
			case <-d.apiReady:
				d.retainQueries(runCtx, *cfg.QueryRetention)
			case <-runCtx.Done():
			}
		})
	}
	return normalizeDaemonExit(ctx, d.Run(runCtx))
}
