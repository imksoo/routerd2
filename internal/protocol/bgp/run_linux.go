//go:build linux

// SPDX-License-Identifier: BSD-3-Clause
package bgp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	gobgpapiutil "github.com/osrg/gobgp/v4/pkg/apiutil"
	gobgpserver "github.com/osrg/gobgp/v4/pkg/server"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GoBGP has process-global best-path selection options. A routerd worker is
// one process per instance; do not accidentally combine two engines in PID 1.
var running atomic.Bool

// Run bounds cancellation even if a GoBGP management operation stops honoring
// its context. On that exceptional deadline the caller must exit this worker
// process: surviving library goroutines cannot safely be reused in-process.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) error {
	if len(cfg.PeerBFD) != 0 {
		return errors.New("BFD-bound BGP requires RunWithBFD and a fenced input stream")
	}
	return runBounded(ctx, cfg, effects, ready, nil)
}

func runBounded(ctx context.Context, cfg Config, effects Effects, ready func(), input *bfdInput) error {
	workerCtx, abort := context.WithCancelCause(ctx)
	defer abort(context.Canceled)
	done := make(chan error, 1)
	go func() { done <- runWorker(workerCtx, cfg, effects, ready, abort, input) }()
	finish := func(err error) error {
		cause := context.Cause(workerCtx)
		if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
			return errors.Join(err, cause)
		}
		return err
	}
	select {
	case err := <-done:
		return finish(err)
	case <-workerCtx.Done():
		timer := time.NewTimer(16 * time.Second)
		defer timer.Stop()
		select {
		case err := <-done:
			return finish(err)
		case <-timer.C:
			return finish(errors.New("BGP worker cancellation exceeded deadline; process exit is required"))
		}
	}
}

func runWorker(ctx context.Context, cfg Config, effects Effects, ready func(), abort context.CancelCauseFunc, input *bfdInput) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("BGP ownership effects are required")
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	if !running.CompareAndSwap(false, true) {
		return errors.New("one BGP module is allowed per worker process")
	}
	defer running.Store(false)
	cfg = Normalize(cfg)
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return err
	}
	fd, err := unix.Open(cfg.StatePath+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), cfg.StatePath+".lock")
	defer lock.Close()
	fi, err := lock.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		return errors.New("BGP state lock must be a private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("BGP applied state is already owned: %w", err)
	}
	listener, err := listenPrivate(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if cfg.GRPCSocketPath != "" {
		if err := privateSocketPath(cfg.GRPCSocketPath); err != nil {
			return err
		}
	}
	if err := initializeState(cfg); err != nil {
		return err
	}
	identity := make([]byte, 16)
	if _, err := rand.Read(identity); err != nil {
		return err
	}
	fatal := make(chan error, 1)
	monitor := &observation{cfg: cfg, epoch: hex.EncodeToString(identity), effects: effects, fail: func(err error) {
		select {
		case fatal <- err:
		default:
		}
		abort(err)
	}}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	options := []gobgpserver.ServerOption{gobgpserver.LoggerOption(slog.Default().With("component", "gobgp"), nil)}
	if cfg.GRPCSocketPath != "" {
		options = append(options, gobgpserver.GrpcListenAddress("unix://"+cfg.GRPCSocketPath), gobgpserver.GrpcOption([]grpc.ServerOption{grpc.MaxRecvMsgSize(2 << 20), grpc.MaxConcurrentStreams(32)}))
	}
	server := gobgpserver.NewBgpServer(options...)
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); server.Serve() }()
	paths := &localPathServer{BgpServer: server}
	var appliedMu sync.Mutex
	api := httpServer(controlHandler(cfg.StatePath, paths, &appliedMu, monitor))
	apiDone := make(chan error, 1)
	apiStarted := false
	gateView := &bfdGateView{}
	var gateDone chan struct{}
	defer func() {
		cancel()
		if gateDone != nil {
			<-gateDone
		}
		// Stop new control mutations before withdrawing the worker's FIB lease.
		if apiStarted {
			stop, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = api.Shutdown(stop)
			stopCancel()
			_ = api.Close()
		}
		var stopErr error
		if input != nil {
			// On gate failure close actual protocol sessions before any slow
			// route-effect withdrawal can delay shutdown.
			stopErr = stopEngine(server.Stop, serveDone, 5*time.Second)
		}
		withdraw, withdrawCancel := context.WithTimeout(context.Background(), time.Duration(cfg.EffectTimeoutMilliseconds)*time.Millisecond)
		inactive := Snapshot{Peers: []Peer{}, Routes: []Route{}, BFD: gateView.snapshot()}
		if result != nil {
			inactive.Error = "BGP worker stopped after an error"
		}
		result = errors.Join(result, monitor.publish(withdraw, inactive))
		withdrawCancel()
		if input == nil {
			stopErr = stopEngine(server.Stop, serveDone, 5*time.Second)
		}
		result = errors.Join(result, stopErr)
		if cfg.GRPCSocketPath != "" && stopErr == nil {
			// GoBGP's Unix listener normally unlinks itself; never remove a path
			// of another type if a local operator changed the configured entry.
			if info, err := os.Lstat(cfg.GRPCSocketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(cfg.GRPCSocketPath)
			}
		}
	}()
	if err := restoreApplied(runCtx, server, paths, cfg, slog.Default()); err != nil {
		return err
	}
	if cfg.GRPCSocketPath != "" {
		if err := waitGRPC(runCtx, cfg.GRPCSocketPath); err != nil {
			return err
		}
	}
	dirty := make(chan struct{}, 1)
	var watchEvents atomic.Uint64
	changed := func() {
		watchEvents.Add(1)
		select {
		case dirty <- struct{}{}:
		default:
		}
	}
	gateView.changed = func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}
	if err := server.WatchEvent(runCtx, gobgpserver.WatchEventMessageCallbacks{
		OnBestPath:   func([]*gobgpapiutil.Path, time.Time) { changed() },
		OnPeerUpdate: func(*gobgpapiutil.WatchEventMessage_PeerEvent, time.Time) { changed() },
	}, gobgpserver.WatchBestPath(true), gobgpserver.WatchPeer()); err != nil {
		return err
	}
	if input != nil {
		applied, _, err := readApplied(cfg.StatePath)
		if err != nil {
			return err
		}
		gateDone = make(chan struct{})
		go func() {
			defer close(gateDone)
			if err := runBFDGate(runCtx, cfg, *input, server, pendingDirectPeerTransitions(applied), gateView, bfd.ReadClock); err != nil {
				monitor.fail(err)
			}
		}()
	}
	publish := func() error {
		observe, observeCancel := context.WithTimeout(runCtx, time.Duration(cfg.EffectTimeoutMilliseconds)*time.Millisecond)
		snapshot, disabled, err := gateView.readSnapshot(func() (Snapshot, error) { return readSnapshot(observe, server, cfg) })
		observeCancel()
		if err != nil {
			return err
		}
		snapshot.WatchEvents = watchEvents.Load()
		if err := monitor.publish(runCtx, snapshot); err != nil {
			return err
		}
		gateView.acknowledge(disabled)
		return nil
	}
	if err := publish(); err != nil {
		return err
	}
	apiStarted = true
	go func() { apiDone <- api.Serve(listener) }()
	if ready != nil {
		ready()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case err := <-apiDone:
			if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("BGP control API stopped: %w", err)
		case <-serveDone:
			return errors.New("GoBGP engine stopped unexpectedly")
		case <-dirty:
			// Coalesce update bursts. The watcher callback never blocks on IPC,
			// disk or a route walk, so its upstream queue drains promptly.
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			select {
			case <-dirty:
			default:
			}
			if err := publish(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-ticker.C:
			// Full periodic reconciliation also bounds recovery after any lost
			// watch notification. Refreshes carry a new sequence and lease.
			if err := publish(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func stopEngine(stop func(), serveDone <-chan struct{}, timeout time.Duration) error {
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return errors.New("GoBGP shutdown exceeded deadline; worker process exit is required")
	}
	select {
	case <-serveDone:
		return nil
	case <-timer.C:
		return errors.New("GoBGP Serve did not exit before shutdown deadline")
	}
}

func waitGRPC(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fi, err := os.Lstat(path); err == nil {
			if fi.Mode()&os.ModeSocket == 0 {
				return errors.New("GoBGP API path is not a socket")
			}
			if err := os.Chmod(path, 0600); err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("GoBGP private API did not bind: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	client, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer client.Close()
	api := gobgpapi.NewGoBgpServiceClient(client)
	for {
		probe, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := api.GetBgp(probe, &gobgpapi.GetBgpRequest{})
		probeCancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("GoBGP private API did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
