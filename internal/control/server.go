package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/worker"
)

func ServeNetwork(ctx context.Context, dir, stateDir string, ready func()) error {
	e, err := network.New(network.Options{StatePath: filepath.Join(stateDir, "network.json")})
	if err != nil {
		return err
	}
	defer e.Close()
	return localapi.Serve(ctx, filepath.Join(dir, NetworkSocketName), networkHandler(e), ready)
}

type nativeNetwork interface {
	Apply(context.Context, uint64, network.Desired) (network.Snapshot, error)
	Snapshot(context.Context) (network.Snapshot, error)
}

func networkHandler(e nativeNetwork) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/routes/lookup", func(w http.ResponseWriter, r *http.Request) {
		var req network.RouteLookupRequest
		if err := localapi.Decode(w, r, &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		result, err := network.LookupRoute(req)
		if err != nil {
			localapi.Fail(w, 422, err)
			return
		}
		localapi.JSON(w, 200, result)
	})
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		s, err := networkObservation(r.Context(), e)
		if err != nil {
			observationFailure(w, err)
			return
		}
		writeObservation(w, s)
	})
	networkObservationRoutes(mux, "/v1/observations/", e)
	networkSourceRoute(mux, e)
	mux.HandleFunc("POST /v1/apply", func(w http.ResponseWriter, r *http.Request) {
		var req ApplyRequest
		if err := localapi.Decode(w, r, &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		if req.Revision == 0 {
			localapi.Fail(w, 400, errors.New("revision must be positive"))
			return
		}
		s, err := e.Apply(r.Context(), req.Revision, req.Desired)
		if err != nil {
			localapi.Fail(w, 422, err)
			return
		}
		localapi.JSON(w, 200, ApplyResult{Revision: s.Revision, AttemptedRevision: s.AttemptedRevision, Status: s.Status})
	})
	return mux
}

func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	if e.runtime != nil {
		h := e.runtime.Handler()
		mux.Handle("/v1/services", h)
		mux.Handle("/v1/services/", h)
	}
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) { localapi.JSON(w, 200, buildinfo.Current()) })
	e.configurationViewRoutes(mux)
	e.observationRoutes(mux)
	e.eventStoreRoutes(mux)
	e.derivedRoutes(mux)
	e.protocolEventRoutes(mux)
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) { e.writeConfigurationState(w, e.State(), false) })
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		s := e.health()
		code := 200
		if !s.Healthy {
			code = 503
		}
		localapi.JSON(w, code, s)
	})
	mux.HandleFunc("GET /v1/network", func(w http.ResponseWriter, r *http.Request) {
		s, err := networkObservation(r.Context(), e.network)
		if err != nil {
			observationFailure(w, err)
			return
		}
		writeObservation(w, s)
	})
	networkObservationRoutes(mux, "/v1/network/", e.network)
	mux.HandleFunc("POST /v1/config/commit", func(w http.ResponseWriter, r *http.Request) {
		var req CommitRequest
		if err := config.Decode(http.MaxBytesReader(w, r.Body, config.MaxSize), &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		s, err := e.Commit(r.Context(), req)
		if err != nil {
			code := 422
			if errors.Is(err, ErrConflict) {
				code = 409
			} else if errors.Is(err, ErrInvalid) {
				code = 400
			} else if errors.Is(err, events.ErrCapacity) {
				code = 503
			}
			localapi.Fail(w, code, err)
			return
		}
		e.writeConfigurationState(w, s, true)
	})
	mux.HandleFunc("POST /v1/config/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req ConfirmRequest
		if err := localapi.Decode(w, r, &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		s, err := e.Confirm(req)
		if err != nil {
			code := 422
			if errors.Is(err, ErrConflict) {
				code = 409
			}
			localapi.Fail(w, code, err)
			return
		}
		e.writeConfigurationState(w, s, true)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, err := strconv.ParseUint(r.URL.Query().Get("after_sequence"), 10, 64)
		if err != nil {
			localapi.Fail(w, 400, errors.New("after_sequence is required and must be a non-negative integer"))
			return
		}
		localapi.JSON(w, 200, e.Changes(r.URL.Query().Get("producer_epoch"), after))
	})
	return mux
}

func Serve(ctx context.Context, dir, stateDir, bootstrap string, ready func()) error {
	instanceID := os.Getenv(worker.SupervisorInstanceEnv)
	if !serviceconfig.ValidSupervisorInstance(instanceID) {
		return errors.New("supervisor instance is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store, current, err := config.Open(stateDir, bootstrap)
	if err != nil {
		return err
	}
	networkClient := NewNetworkClient(dir)
	e := New(store, current, networkClient)
	e.journal, err = events.Open(events.Options{Directory: filepath.Join(stateDir, "events")})
	if err != nil {
		return fmt.Errorf("open durable event journal: %w", err)
	}
	defer e.journal.Close()
	if err := e.RefreshEventStore(ctx); err != nil {
		return fmt.Errorf("inspect durable event journal: %w", err)
	}
	e.runtime = runtimeplan.New(dir, stateDir, networkClient, instanceID)
	// Protocol readiness may require an effects acknowledgement. Start the
	// private API before reconciliation, while public health remains false.
	listening := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- localapi.Serve(ctx, filepath.Join(dir, SocketName), e.Handler(), func() { close(listening) })
	}()
	select {
	case <-listening:
	case err := <-serverDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	go e.runtime.Monitor(ctx)
	// The network worker may still be starting. Reconcile before marking control
	// ready; the supervisor's own status API remains available during this wait.
	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if lastErr = e.Reconcile(ctx); lastErr == nil {
			break
		}
		timer.Reset(250 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	monitorCtx, stop := context.WithCancel(ctx)
	defer stop()
	storageDone := make(chan struct{})
	go func() { defer close(storageDone); e.monitorEventStore(monitorCtx) }()
	defer func() { stop(); <-storageDone }()
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			flushCtx, cancel := context.WithTimeout(monitorCtx, 2*time.Second)
			err := e.FlushEvents(flushCtx)
			cancel()
			if err != nil && monitorCtx.Err() == nil {
				slog.Error("durable configuration event delivery failed", "error", err)
			}
			recoveryCtx, cancelRecovery := context.WithTimeout(monitorCtx, 2*time.Second)
			recoveryErr := e.ProcessRetiredProtocolEvents(recoveryCtx)
			cancelRecovery()
			if recoveryErr != nil && monitorCtx.Err() == nil {
				slog.Error("retired protocol history recovery failed", "error", recoveryErr)
			}
			derivedCtx, cancelDerived := context.WithTimeout(monitorCtx, 2*time.Second)
			derivedErr := e.ProcessDerivedEvents(derivedCtx)
			cancelDerived()
			if derivedErr != nil && monitorCtx.Err() == nil {
				slog.Error("durable derived evaluation failed", "error", derivedErr)
			}
			ruleCtx, cancelRules := context.WithTimeout(monitorCtx, 2*time.Second)
			ruleErr := e.ProcessEventRules(ruleCtx, time.Now())
			cancelRules()
			if ruleErr != nil && monitorCtx.Err() == nil {
				slog.Error("durable event rule processing failed", "error", ruleErr)
			}
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { stop(); <-eventDone }()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case now := <-ticker.C:
				if err := e.Expire(monitorCtx, now); err != nil {
					slog.Error("confirmed commit rollback failed", "error", err)
				}
			}
		}
	}()
	ready()
	select {
	case err := <-serverDone:
		return err
	case <-ctx.Done():
		return nil
	}
}
