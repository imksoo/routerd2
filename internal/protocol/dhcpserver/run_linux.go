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
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

// Run owns DHCP protocol sockets and its private journal. Effects is the only
// control-plane publication path; the worker does not install client addresses
// or mutate DNS directly. A response granting a lease is sent after local
// fsync and a bounded Effects.Update. Standby replication runs independently.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) error {
	return run(ctx, cfg, effects, ready, func(c Config) (transport, error) { return openTransport(c) })
}

func run(ctx context.Context, cfg Config, effects Effects, ready func(), open func(Config) (transport, error)) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("DHCP server effects are required")
	}
	cfg = Normalize(cfg)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, path := range []string{cfg.SocketPath, cfg.StatusPath, cfg.EventPath} {
		if path != "" {
			if err := privateParent(path); err != nil {
				return err
			}
		}
	}
	store, j, err := openStore(cfg, time.Now().UTC())
	if err != nil {
		return err
	}
	defer store.Close()
	var sync *replication
	var requests <-chan replicationRequest
	var replicationStopped <-chan struct{}
	var updates chan replicationUpdate
	var outcomes <-chan replicationOutcome
	var reconciliations <-chan reconciliation
	var stopReconciliation context.CancelFunc
	var syncStatus dhcpserverconfig.SyncStatus
	var blocked bool
	if cfg.Replication != nil {
		sync, err = startReplication(ctx, cfg)
		if err != nil {
			return err
		}
		defer sync.close()
		requests = sync.requests
		replicationStopped = sync.done
		if !cfg.Standby {
			owner := cfg.Replication.ListenAddress.String()
			remote, err := sync.pull(ctx, cfg)
			if err != nil {
				if !peerUnavailable(err) || ctx.Err() != nil {
					return fmt.Errorf("read standby DHCP journal before serving: %w", err)
				}
				syncStatus.Error = err.Error()
				blocked = j.Owner != owner && !(j.Owner == "" && cfg.Bootstrap)
				if !blocked {
					j.Owner = owner
				}
			} else {
				now := time.Now().UTC()
				merged, mergeErr := reconcileJournals(j, remote, now, cfg.MaxLeases)
				if mergeErr != nil {
					blocked = true
					syncStatus.Error = mergeErr.Error()
				} else {
					j = merged
				}
				if !blocked && (j.Owner != owner || remote.Owner != owner) {
					candidate := j
					candidate.Owner = owner
					candidate.Revision++
					candidate.WrittenAt = now
					data, encodeErr := encodeJournal(candidate)
					if encodeErr != nil {
						return encodeErr
					}
					if _, err := sync.exchange(ctx, http.MethodPut, data); err != nil {
						blocked = true
						syncStatus.Error = err.Error()
					} else {
						if err := store.Save(candidate); err != nil {
							return fmt.Errorf("persist DHCP ownership: %w", err)
						}
						j = candidate
						syncStatus.AcknowledgedRevision = j.Revision
					}
				}
			}
			syncStatus.AllocationBlocked = blocked
			updates = make(chan replicationUpdate, 1)
			results := make(chan replicationOutcome, 1)
			outcomes = results
			pushCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); sync.push(pushCtx, updates, results) }()
			defer func() { cancel(); <-done }()
			if blocked {
				pollCtx, stop := context.WithCancel(ctx)
				stopReconciliation = stop
				pollResults := make(chan reconciliation, 1)
				reconciliations = pollResults
				pollDone := make(chan struct{})
				go func() { defer close(pollDone); sync.poll(pollCtx, cfg, pollResults) }()
				defer func() { stop(); <-pollDone }()
			}
		}
	}
	persist := func(j journal) error {
		data, err := encodeJournal(j)
		if err != nil {
			return err
		}
		if err := writePrivate(store.path, data); err != nil {
			return err
		}
		if updates != nil && !blocked {
			update := replicationUpdate{revision: j.Revision, data: data}
			select {
			case updates <- update:
			default:
				select {
				case <-updates:
				default:
				}
				updates <- update
			}
		}
		return nil
	}
	e := newEngine(cfg, j, func() time.Time { return time.Now().UTC() }, persist)
	e.blockNew = blocked
	var conn transport
	if !cfg.Standby {
		conn, err = open(cfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		e.probe = conn.Probe
		if t, ok := conn.(*nativeTransport); ok {
			e.mtu = t.mtu
		}
		if err = e.sweep(e.now()); err != nil {
			return err
		}
	}
	if len(e.leases) > cfg.MaxLeases || len(e.quarantines) > cfg.MaxLeases {
		return errors.New("DHCP journal exceeds the configured max_leases; retained leases cannot be discarded")
	}
	if !cfg.Standby {
		if err = e.save(e.now()); err != nil {
			return err
		}
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	epoch := hex.EncodeToString(id[:])
	monitor := &observation{epoch: epoch}
	var seq uint64
	var lastExpiresNanos uint64
	publish := func(ctx context.Context, active bool, reason error) error {
		seq++
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		s := e.snapshot(epoch, seq, clock, active)
		if updates != nil {
			status := syncStatus
			s.Sync = &status
		}
		if reason != nil {
			s.Error = reason.Error()
		}
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if len(b) > dhcpserverconfig.MaxSnapshotBytes {
			return errors.New("DHCP snapshot exceeds its IPC bound")
		}
		effectCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.EffectTimeoutMilliseconds)*time.Millisecond)
		err = effects.Update(effectCtx, s)
		cancel()
		if err == nil && active {
			clock, err = boottime.ReadForBoot(s.BootID)
			if err == nil && !dhcpserverconfig.CurrentObservation(s, clock) {
				err = errors.New("DHCP effects acknowledgment arrived after its original observation expired")
			}
		}
		if err != nil {
			return fmt.Errorf("publish DHCP lease snapshot: %w", err)
		}
		lastExpiresNanos = s.ExpiresNanos
		monitor.setSnapshot(s)
		if cfg.StatusPath != "" {
			if err := writePrivate(cfg.StatusPath, append(b, '\n')); err != nil {
				return err
			}
		}
		for _, event := range e.events {
			event = monitor.addEvent(event)
			if cfg.LogDHCP {
				attrs := []any{"resource", cfg.Resource, "event", event.Type, "sequence", event.Sequence}
				if event.Lease != nil {
					attrs = append(attrs, "address", event.Lease.IPAddress, "hardware_address", event.Lease.MACAddress, "hostname", event.Lease.Hostname, "scope", event.Lease.Scope)
				}
				slog.Info("dhcp.server.event", attrs...)
			}
			if cfg.EventPath != "" {
				if err := appendEvent(cfg.EventPath, event); err != nil {
					return err
				}
			}
		}
		e.events = nil
		return nil
	}
	defer func() {
		e.events = append(e.events, Event{Type: "stopped", Resource: cfg.Resource, At: e.now()})
		cleanup, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.EffectTimeoutMilliseconds)*time.Millisecond)
		defer cancel()
		if err := publish(cleanup, false, result); err != nil {
			result = errors.Join(result, err)
		}
	}()
	e.events = append(e.events, Event{Type: "started", Resource: cfg.Resource, At: e.now()})
	if !cfg.Standby {
		if err = conn.Check(); err != nil {
			return err
		}
	}
	if err = publish(ctx, !cfg.Standby, nil); err != nil {
		return err
	}

	var apiDone <-chan error
	if cfg.SocketPath != "" {
		listener, cleanup, err := listenPrivate(cfg.SocketPath)
		if err != nil {
			return err
		}
		defer cleanup()
		server := &http.Server{Handler: monitor.handler(), ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
		done := make(chan error, 1)
		apiDone = done
		go func() {
			err := server.Serve(listener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			done <- err
		}()
		defer server.Close()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	packets := make(chan packet, 64)
	receiveError := make(chan error, 1)
	receiveDone := make(chan struct{})
	var dropped atomic.Uint64
	go func() {
		defer close(receiveDone)
		if cfg.Standby {
			return
		}
		for {
			p, err := conn.Receive(runCtx)
			if err != nil {
				select {
				case receiveError <- err:
				default:
				}
				return
			}
			select {
			case packets <- p:
			case <-runCtx.Done():
				return
			default:
				dropped.Add(1)
			}
		}
	}()
	// Native Receive checks its context at least every 200 ms. Waiting before
	// Close keeps transport fields and descriptor reuse race-free.
	defer func() { cancel(); <-receiveDone }()
	if ready != nil {
		ready()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var reportedDrops uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-replicationStopped:
			return fmt.Errorf("DHCP replication listener stopped: %w", sync.err)
		case outcome := <-outcomes:
			if outcome.err != nil {
				syncStatus.Error = outcome.err.Error()
			} else {
				syncStatus.AcknowledgedRevision = outcome.revision
				syncStatus.Error = ""
			}
		case outcome := <-reconciliations:
			if outcome.err != nil {
				syncStatus.Error = outcome.err.Error()
				continue
			}
			merged, err := reconcileJournals(e.journal(), outcome.journal, e.now(), cfg.MaxLeases)
			if err != nil {
				syncStatus.Error = err.Error()
				continue
			}
			merged.Owner = cfg.Replication.ListenAddress.String()
			merged.Revision++
			merged.WrittenAt = e.now()
			data, err := encodeJournal(merged)
			if err == nil {
				_, err = sync.exchange(ctx, http.MethodPut, data)
			}
			if err != nil {
				syncStatus.Error = err.Error()
				continue
			}
			if err := store.Save(merged); err != nil {
				return fmt.Errorf("persist reconciled DHCP journal: %w", err)
			}
			e.restore(merged)
			e.blockNew, blocked = false, false
			syncStatus = dhcpserverconfig.SyncStatus{AcknowledgedRevision: merged.Revision}
			stopReconciliation()
			reconciliations = nil
			if err := publish(ctx, true, nil); err != nil {
				return err
			}
		case req := <-requests:
			if err := ctx.Err(); err != nil {
				return err
			}
			var result replicationResult
			if req.write {
				result.err = applyReplica(e, store, req.state)
			} else {
				result.data, result.err = encodeJournal(e.journal())
			}
			req.done <- result
			if req.write && result.err == nil {
				if err := publish(ctx, false, nil); err != nil {
					return err
				}
			}
		case err := <-apiDone:
			if err == nil {
				err = errors.New("DHCP private API closed unexpectedly")
			}
			return err
		case err := <-receiveError:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("receive DHCP request: %w", err)
		case <-ticker.C:
			if !cfg.Standby {
				if err := conn.Check(); err != nil {
					return err
				}
				if err := e.sweep(e.now()); err != nil {
					return err
				}
			}
			n := dropped.Load()
			e.stats.Rejected += n - reportedDrops
			reportedDrops = n
			if err := publish(ctx, !cfg.Standby, nil); err != nil {
				return err
			}
		case p := <-packets:
			if err := e.sweep(e.now()); err != nil {
				return err
			}
			reply, err := e.handle(ctx, p)
			if err != nil {
				return err
			}
			clock, err := boottime.Read()
			if err != nil {
				return err
			}
			if len(e.events) > 0 || reply != nil && uint64(clock.BoottimeNanoseconds)+uint64(time.Second) >= lastExpiresNanos {
				if err := publish(ctx, true, nil); err != nil {
					return err
				}
			}
			if reply != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				clock, err := boottime.Read()
				if err != nil {
					return err
				}
				if uint64(clock.BoottimeNanoseconds) >= lastExpiresNanos {
					return errors.New("DHCP original observation expired before reply")
				}
				if err := conn.Send(ctx, *reply); err != nil {
					return fmt.Errorf("send DHCP reply: %w", err)
				}
			}
		}
	}
}
