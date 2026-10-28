// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"golang.org/x/sys/unix"
)

type engineSettings struct {
	Intent      Config  `json:"intent"`
	Restricted  bool    `json:"restricted,omitempty"`
	Bindings    []Lease `json:"bindings,omitempty"`
	EventSocket string  `json:"event_socket,omitempty"`
}

type engineProcess struct {
	child  *exec.Cmd
	done   chan error
	exited bool
}

// dnsmasq flushes its script queue on TERM, but its helper can outlive the
// main process. This worker is their subreaper and keeps saving events until
// every child has exited. Cleanup has its own bounded lifetime after cancel.
func (p *engineProcess) close(ctx context.Context, cfg Config, events *leaseEvents) error {
	var requests <-chan leaseEventCall
	if events != nil {
		requests = events.requests
	}
	var mainDone <-chan error
	if !p.exited {
		_ = p.child.Process.Signal(syscall.SIGTERM)
		mainDone = p.done
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var firstErr error
	for {
		if p.exited {
			for {
				pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
				if errors.Is(err, unix.ECHILD) {
					return firstErr
				}
				if err != nil {
					return err
				}
				if pid == 0 {
					break
				}
			}
		}
		select {
		case <-mainDone:
			p.exited = true
			mainDone = nil
		case call := <-requests:
			err := recordLeaseEvent(ctx, cfg, call.event)
			call.done <- err
			if firstErr == nil {
				firstErr = err
			}
		case <-ticker.C:
		case <-ctx.Done():
			if !p.exited {
				_ = p.child.Process.Kill()
				<-p.done
				p.exited = true
			}
			return fmt.Errorf("drain dnsmasq lease notifications: %w", ctx.Err())
		}
	}
}

func startEngine(ctx context.Context, settings engineSettings, image *os.File, events *leaseEvents) (_ *engineProcess, startErr error) {
	fd, err := unix.MemfdCreate("dnsmasq-config", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	config := os.NewFile(uintptr(fd), "dnsmasq-config")
	defer config.Close()
	if err := json.NewEncoder(config).Encode(settings); err != nil {
		return nil, err
	}
	if _, err := config.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// PDEATHSIG is attached to the creating thread, which owns this child
	// until it is reaped, including cancellation and startup failures.
	child := exec.Command("/proc/self/exe", "dnsmasq-engine")
	child.ExtraFiles = []*os.File{config, image}
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	log := slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo).Writer()
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		return nil, err
	}
	p := &engineProcess{child: child, done: make(chan error, 1)}
	go func() { p.done <- child.Wait() }()
	success := false
	defer func() {
		if !success {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := p.close(cleanup, settings.Intent, events); err != nil {
				startErr = fmt.Errorf("DHCP startup cleanup: %w (startup: %v)", err, startErr)
			}
		}
	}()
	pidfile := filepath.Join(filepath.Dir(settings.Intent.StatePath), "dnsmasq.pid")
	startup := time.NewTimer(5 * time.Second)
	defer startup.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-p.done:
			p.exited = true
			return nil, fmt.Errorf("dnsmasq startup exited: %v", err)
		case <-startup.C:
			return nil, errors.New("dnsmasq did not create its PID file")
		case <-ticker.C:
			pid, err := os.ReadFile(pidfile)
			if err == nil && strings.TrimSpace(string(pid)) == strconv.Itoa(child.Process.Pid) {
				success = true
				return p, nil
			}
		}
	}
}

// Run owns process lifetime and observes dnsmasq's lease file. A standby
// receives that same file and opens no DHCP socket.
func Run(ctx context.Context, cfg Config, image *os.File, publish func(context.Context, Snapshot) error, ready func()) (runErr error) {
	if err := dhcpserverconfig.Validate(cfg); err != nil {
		return err
	}
	cfg = dhcpserverconfig.Normalize(cfg)
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("claim DHCP state: %w", err)
	}
	if cfg.StickyHoldDays > 0 {
		state, err := readTransfer(ctx, cfg)
		if err != nil {
			return err
		}
		if err := saveHistory(ctx, cfg, state.History); err != nil {
			return err
		}
		active, err := mergeLeases(state.Leases, "", state.History, time.Now())
		if err != nil {
			return err
		}
		if active != state.Leases {
			if err := replaceFile(cfg.StatePath, []byte(active)); err != nil {
				return err
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	var replica *replication
	if cfg.Replication != nil {
		replica, err = startReplication(ctx, cfg)
		if err != nil {
			return err
		}
		defer replica.close()
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return err
	}
	epochText := hex.EncodeToString(epoch[:])
	var sequence uint64
	syncStatus := dhcpserverconfig.SyncStatus{}
	var child *engineProcess
	// PDEATHSIG belongs to this creating thread throughout child lifetime.
	if !cfg.Standby {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	var events *leaseEvents
	defer func() {
		if child != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := child.close(cleanup, cfg, events); err != nil {
				runErr = fmt.Errorf("DHCP cleanup: %w (service: %v)", err, runErr)
			} else if replica != nil && !cfg.Standby {
				state, err := readTransfer(cleanup, cfg)
				if err == nil {
					_, err = replica.exchange(cleanup, http.MethodPut, state)
				}
				if err != nil {
					slog.Warn("final DHCP lease synchronization failed", "error", err)
				}
			}
		}
		if events != nil {
			events.close()
		}
	}()
	claim := func(peer leaseTransfer) error {
		state, err := readTransfer(ctx, cfg)
		if err != nil {
			return err
		}
		state.History = mergeHistory(state.History, peer.History)
		state.Leases, err = mergeLeases(state.Leases, peer.Leases, state.History, time.Now())
		if err != nil {
			return err
		}
		state.Owner = cfg.Replication.ListenAddress.String()
		if _, err := replica.exchange(ctx, http.MethodPut, state); err != nil {
			return err
		}
		if err := saveTransfer(ctx, cfg, state); err != nil {
			return err
		}
		syncStatus.Synchronized = true
		syncStatus.AllocationBlocked = false
		syncStatus.Error = ""
		return nil
	}
	start := func() error {
		if events != nil {
			events.close()
			events = nil
		}
		if cfg.StickyHoldDays > 0 {
			events, err = listenLeaseEvents(ctx, cfg)
			if err != nil {
				return err
			}
		}
		if _, err := updateHeldHosts(ctx, cfg); err != nil {
			return err
		}
		bindings, err := readDNSMasqLeases(cfg, time.Now())
		if err != nil {
			return err
		}
		settings := engineSettings{Intent: cfg, Restricted: syncStatus.AllocationBlocked, Bindings: bindings}
		if events != nil {
			settings.EventSocket = "/state/" + events.name
		}
		child, err = startEngine(ctx, settings, image, events)
		return err
	}
	if !cfg.Standby {
		if replica != nil {
			peer, exchangeErr := replica.exchange(ctx, http.MethodGet, leaseTransfer{})
			if exchangeErr == nil {
				if err := claim(peer); err != nil {
					return err
				}
			} else {
				state, err := readTransfer(ctx, cfg)
				if err != nil {
					return err
				}
				syncStatus.Error = exchangeErr.Error()
				syncStatus.AllocationBlocked = state.Owner != cfg.Replication.ListenAddress.String()
				_, stateErr := os.Stat(cfg.StatePath)
				if errors.Is(stateErr, os.ErrNotExist) && cfg.Bootstrap {
					state.Owner = cfg.Replication.ListenAddress.String()
					if err := saveTransfer(ctx, cfg, state); err != nil {
						return err
					}
					syncStatus.AllocationBlocked = false
				}
			}
		}
		if err := start(); err != nil {
			return err
		}
	}

	update := func() error {
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		sequence++
		snapshot := Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, ServerAddress: cfg.ServerAddress, Epoch: epochText, Sequence: sequence, Active: !cfg.Standby, Standby: cfg.Standby, ObservedAt: clock.WallTime, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(dhcpserverconfig.ObservationLifetime)}
		if !cfg.Standby {
			snapshot.Leases, err = readDNSMasqLeases(cfg, clock.WallTime)
			if err != nil {
				return err
			}
			if replica != nil {
				copy := syncStatus
				snapshot.Sync = &copy
			}
		}
		call, cancel := context.WithTimeout(ctx, time.Duration(cfg.EffectTimeoutMilliseconds)*time.Millisecond)
		defer cancel()
		return publish(call, snapshot)
	}
	if err := update(); err != nil {
		return err
	}
	ready()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var childDone <-chan error
	if child != nil {
		childDone = child.done
	}
	var replicaDone <-chan struct{}
	if replica != nil {
		replicaDone = replica.done
	}
	var eventRequests <-chan leaseEventCall
	var eventDone <-chan struct{}
	if events != nil {
		eventRequests, eventDone = events.requests, events.done
	}
	for {
		select {
		case call := <-eventRequests:
			err := recordLeaseEvent(ctx, cfg, call.event)
			if err == nil {
				var changed bool
				changed, err = updateHeldHosts(ctx, cfg)
				if err == nil && changed {
					err = child.child.Process.Signal(syscall.SIGHUP)
				}
			}
			call.done <- err
			if err != nil {
				return err
			}
		case <-eventDone:
			return fmt.Errorf("DHCP event listener: %w", events.err)
		case <-ctx.Done():
			return ctx.Err()
		case err := <-childDone:
			child.exited = true
			return fmt.Errorf("dnsmasq stopped: %v", err)
		case <-replicaDone:
			return fmt.Errorf("DHCP replication listener: %w", replica.err)
		case <-ticker.C:
			if child != nil {
				changed, err := updateHeldHosts(ctx, cfg)
				if err != nil {
					return err
				}
				if changed {
					if err := child.child.Process.Signal(syscall.SIGHUP); err != nil {
						return err
					}
				}
			}
			if replica != nil && !cfg.Standby {
				if syncStatus.AllocationBlocked {
					peer, exchangeErr := replica.exchange(ctx, http.MethodGet, leaseTransfer{})
					if exchangeErr == nil {
						// Only handoff changes the file or serving mode; ordinary link
						// outages leave the known-client engine running.
						cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
						err := child.close(cleanup, cfg, events)
						cancel()
						child = nil
						if err != nil {
							return err
						}
						if err := claim(peer); err != nil {
							return err
						}
						if err := start(); err != nil {
							return err
						}
						childDone = child.done
						if events != nil {
							eventRequests, eventDone = events.requests, events.done
						}
					} else {
						syncStatus.Error = exchangeErr.Error()
					}

				} else {
					state, err := readTransfer(ctx, cfg)
					if err != nil {
						return err
					}
					_, err = replica.exchange(ctx, http.MethodPut, state)
					syncStatus.Synchronized = err == nil
					syncStatus.Error = ""
					if err != nil {
						syncStatus.Error = err.Error()
					}
				}
			}
			if err := update(); err != nil {
				return err
			}
		}
	}
}
