package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

func ServeNetwork(ctx context.Context, dir, stateDir string, ready func()) error {
	e, err := network.New(network.Options{StatePath: filepath.Join(stateDir, "network.json")})
	if err != nil {
		return err
	}
	defer e.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		s, err := e.Snapshot(r.Context())
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		localapi.JSON(w, 200, s)
	})
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
		localapi.JSON(w, 200, s)
	})
	return localapi.Serve(ctx, filepath.Join(dir, NetworkSocketName), mux, ready)
}

func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) { localapi.JSON(w, 200, buildinfo.Current()) })
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) { localapi.JSON(w, 200, e.State()) })
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		s := e.State()
		code := 200
		if !s.Healthy {
			code = 503
		}
		localapi.JSON(w, code, s)
	})
	mux.HandleFunc("GET /v1/network", func(w http.ResponseWriter, r *http.Request) {
		s, err := e.network.Snapshot(r.Context())
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		localapi.JSON(w, 200, s)
	})
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
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, s)
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
		localapi.JSON(w, 200, s)
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
	store, current, err := config.Open(stateDir, bootstrap)
	if err != nil {
		return err
	}
	e := New(store, current, NewNetworkClient(dir))
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
	return localapi.Serve(ctx, filepath.Join(dir, SocketName), e.Handler(), ready)
}
