//go:build linux

package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/statefs"
	"github.com/imksoo/routerd2/internal/telemetry"
	"golang.org/x/sys/unix"
)

// Run owns the fixed daemon and its kernel scope. Control supplies the applied
// source's original authority; polling cannot rebase a condition deadline.
func Run(ctx context.Context, cfg tailscaleconfig.Config, image *os.File, input func(context.Context) (tailscaleconfig.Input, error), observe func(context.Context, tailscaleconfig.Snapshot) error, ready func()) (result error) {
	if err := tailscaleconfig.Validate(cfg); err != nil {
		return err
	}
	state, err := statefs.OpenDirectory(cfg.StateDir, true)
	if err != nil {
		return err
	}
	defer state.Close()
	run, err := statefs.OpenDirectory(cfg.RuntimeDir, true)
	if err != nil {
		return err
	}
	defer run.Close()
	lockFD, err := unix.Openat(int(state.Fd()), "engine.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(lockFD), "tailscale-lock")
	defer lock.Close()
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("Tailscale engine is already owned: %w", err)
	}
	var key string
	if cfg.AuthKeyFile != "" {
		dir, name, err := statefs.OpenParent(cfg.AuthKeyFile, false)
		if err != nil {
			return err
		}
		raw, err := statefs.ReadPrivate(ctx, dir, name, 4096)
		dir.Close()
		if err != nil {
			return err
		}
		key = strings.TrimSpace(string(raw))
		clear(raw)
	}
	request, err := json.Marshal(startOptions(cfg.Node, key))
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Abstract Unix sockets are scoped to the network namespace. Unlike a
	// state-directory lock, this also excludes another router using another
	// identity directory while claiming the same table52 and TUN namespace.
	namespace, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(namespace)
	if err = unix.Bind(namespace, &unix.SockaddrUnix{Name: "@routerd2.tailscale-owner"}); err != nil {
		return fmt.Errorf("claim Tailscale namespace: %w", err)
	}
	cookie, err := unix.GetsockoptUint64(namespace, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil {
		return err
	}
	var engine *process
	var cleanup func() error
	stop := func() error {
		if engine != nil {
			engine.close()
			engine = nil
		}
		if cleanup != nil {
			err := cleanup()
			cleanup = nil
			return err
		}
		return nil
	}
	defer func() { result = errors.Join(result, stop()) }()
	// Process readiness is not tailnet connectivity. Publishing the actual
	// backend state below allows callers to distinguish them.
	ready()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var current tailscaleconfig.Input
	var nextPeerMeasurement uint64
	for {
		if ctx.Err() != nil {
			return nil
		}
		clock, clockErr := boottime.Read()
		if clockErr != nil {
			return clockErr
		}
		timeout := 2 * time.Second
		if engine != nil {
			timeout = min(timeout, current.Authority.Remaining(clock))
		}
		call, cancel := context.WithTimeout(ctx, timeout)
		next, err := input(call)
		cancel()
		if err == nil {
			current = next
		}
		clock, clockErr = boottime.Read()
		if clockErr != nil {
			return clockErr
		}
		if current.ConfigDigest == "" || !current.Authority.Live(clock) {
			if err := stop(); err != nil {
				return err
			}
		} else {
			if engine == nil {
				cleanup, err = claimKernel(ctx, state, cfg.Interface, cookie)
				if err != nil {
					return err
				}
				engine, err = start(ctx, cfg, image)
				if err != nil {
					return err
				}
				clock, err = boottime.Read()
				if err != nil {
					return err
				}
				if !current.Authority.Live(clock) {
					if err = stop(); err != nil {
						return err
					}
					continue
				}
				call, cancel := context.WithTimeout(ctx, min(2*time.Second, current.Authority.Remaining(clock)))
				_, err = engine.request(call, "POST", "start", bytes.NewReader(request))
				cancel()
				if err != nil {
					return err
				}
			}
			select {
			case <-engine.done:
				return errors.Join(errors.New("tailscaled exited"), engine.err)
			default:
			}
			call, cancel := context.WithTimeout(ctx, min(2*time.Second, current.Authority.Remaining(clock)))
			data, err := engine.request(call, "GET", "status", nil)
			cancel()
			if err != nil {
				return err
			}
			snapshot, err := parseStatus(data)
			if err != nil {
				return err
			}
			clock, err = boottime.Read()
			if err != nil {
				return err
			}
			if current.Authority.Live(clock) {
				snapshot.Resource, snapshot.Interface = cfg.Resource, cfg.Interface
				snapshot.BootID = clock.BootID
				snapshot.ObservedNanos = uint64(clock.BoottimeNanoseconds)
				snapshot.ExpiresNanos = snapshot.ObservedNanos + uint64(min(3*time.Second, current.Authority.Remaining(clock)))
				// Periodic successful samples also restore an unchanged gauge after
				// the exporter restarts. Reuse this observation; export owns no polling.
				if snapshot.ObservedNanos >= nextPeerMeasurement {
					count := 0
					for _, peer := range snapshot.Peers {
						if peer.Online {
							count++
						}
					}
					telemetry.TailscalePeers{Resource: cfg.Resource, OnlinePeers: count}.Record(ctx)
					nextPeerMeasurement = snapshot.ObservedNanos + uint64(10*time.Second)
				}
				call, cancel := context.WithTimeout(ctx, min(2*time.Second, current.Authority.Remaining(clock)))
				err = observe(call, snapshot)
				cancel()
				if err != nil {
					// Reporting does not revoke the engine's original authority.
					// The next iteration still stops it if that grant expired.
					slog.Warn("publish Tailscale observation", "error", err)
				}
			} else if err = stop(); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
