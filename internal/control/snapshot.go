package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
)

type SnapshotRequest struct {
	ExpectedRevision uint64 `json:"expected_revision"`
}

// Snapshot shares the configuration writer lock; it cannot capture the middle
// of a commit/confirmation/rollback and does not change the kernel or journal.
func (e *Engine) Snapshot(r SnapshotRequest) (config.SnapshotRef, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.ExpectedRevision == 0 || r.ExpectedRevision != e.state.Revision || e.state.Confirmation != nil {
		return config.SnapshotRef{}, ErrConflict
	}
	if !e.state.Healthy {
		return config.SnapshotRef{}, errors.New("configuration must be reconciled before snapshot capture")
	}
	return e.store.Snapshot(r.ExpectedRevision)
}

// RestoreRequest selects an immutable saved intent. It never replaces the
// current journal, revision, runtime identity or release selection.
type RestoreRequest struct {
	Snapshot              config.SnapshotRef `json:"snapshot"`
	ExpectedRevision      uint64             `json:"expected_revision"`
	RequestID             string             `json:"request_id"`
	ConfirmTimeoutSeconds int                `json:"confirm_timeout_seconds,omitempty"`
}

func (e *Engine) Restore(ctx context.Context, r RestoreRequest) (State, error) {
	saved, err := e.store.ResolveSnapshot(r.Snapshot)
	if err != nil {
		return State{}, fmt.Errorf("resolve saved configuration: %w", err)
	}
	// Resolve the immutable file without holding the configuration writer lock.
	// Commit owns revision/replay checks, application, persistence and recovery.
	return e.Commit(ctx, CommitRequest{ExpectedRevision: r.ExpectedRevision,
		RequestID: r.RequestID, Config: saved.Config, ConfirmTimeoutSeconds: r.ConfirmTimeoutSeconds})
}

func (e *Engine) snapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/config/snapshots", func(w http.ResponseWriter, r *http.Request) {
		result, err := e.store.ListSnapshots()
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		localapi.JSON(w, 200, result)
	})
	mux.HandleFunc("POST /v1/config/restore", func(w http.ResponseWriter, r *http.Request) {
		var req RestoreRequest
		if err := config.Decode(http.MaxBytesReader(w, r.Body, config.MaxSize), &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		state, err := e.Restore(r.Context(), req)
		if err != nil {
			code := 422
			if errors.Is(err, ErrConflict) {
				code = 409
			} else if errors.Is(err, ErrInvalid) {
				code = 400
			} else if errors.Is(err, events.ErrCapacity) || errors.Is(err, release.ErrBusy) {
				code = 503
			}
			localapi.Fail(w, code, err)
			return
		}
		e.writeConfigurationState(w, state, true)
	})
	mux.HandleFunc("POST /v1/config/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var req SnapshotRequest
		if err := config.Decode(http.MaxBytesReader(w, r.Body, config.MaxSize), &req); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		ref, err := e.Snapshot(req)
		if err != nil {
			code := 422
			if errors.Is(err, ErrConflict) || errors.Is(err, config.ErrSnapshotConflict) {
				code = 409
			}
			if errors.Is(err, config.ErrSnapshotCapacity) || errors.Is(err, config.ErrSnapshotUncertain) {
				code = 503
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, ref)
	})
}
