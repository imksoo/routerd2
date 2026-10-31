package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
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

type SnapshotDeleteRequest struct {
	ExpectedRevision uint64             `json:"expected_revision"`
	Snapshot         config.SnapshotRef `json:"snapshot"`
}

var ErrSnapshotSelected = errors.New("configuration snapshot is in use by the current configuration or a boot selection")

// Deletion uses the same writer/release lock order as configuration commits.
// A prepare, boot, or rollback cannot select a reference while it is removed.
func (e *Engine) DeleteSnapshot(ctx context.Context, r SnapshotDeleteRequest) error {
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.ExpectedRevision == 0 || r.ExpectedRevision != e.state.Revision || e.state.Confirmation != nil {
		return ErrConflict
	}
	if r.Snapshot.StoreID != e.state.StoreID {
		return errors.New("snapshot belongs to a different configuration store")
	}
	selected := func(s *configref.Selection) bool {
		return s != nil && (s.Snapshot == r.Snapshot || s.Recovery != nil && s.Recovery.FailedSnapshot == r.Snapshot)
	}
	if r.Snapshot.Revision == e.state.Revision || e.state.Startup != nil && selected(&e.state.Startup.Selection) {
		return ErrSnapshotSelected
	}
	remove := func(state release.State) error {
		if selected(state.Startup) || selected(state.PreviousStartup) || state.Pending != nil && (selected(state.Pending.Startup) || selected(state.Pending.Fallback)) {
			return ErrSnapshotSelected
		}
		return e.store.DeleteSnapshot(r.Snapshot)
	}
	if e.releaseStore != nil {
		return e.releaseStore.WithState(ctx, func(state release.State) error {
			if err := selectedConfiguration(state, e.releaseDigest, e.state.Committed); err != nil {
				return err
			}
			return remove(state)
		})
	}
	return remove(release.State{})
}

func (e *Engine) snapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/config/snapshots/export", func(w http.ResponseWriter, r *http.Request) {
		var ref config.SnapshotRef
		if err := localapi.Decode(w, r, &ref); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		// Resolve the existing immutable copy without applying it or holding
		// the live configuration writer while reading historical intent.
		saved, err := e.store.ResolveSnapshot(ref)
		if err != nil {
			localapi.Fail(w, http.StatusUnprocessableEntity, err)
			return
		}
		localapi.JSON(w, http.StatusOK, saved.Config)
	})

	mux.HandleFunc("POST /v1/config/snapshots/delete", func(w http.ResponseWriter, r *http.Request) {
		var request SnapshotDeleteRequest
		if err := localapi.Decode(w, r, &request); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		if err := e.DeleteSnapshot(r.Context(), request); err != nil {
			code := http.StatusUnprocessableEntity
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrSnapshotSelected) {
				code = http.StatusConflict
			} else if errors.Is(err, release.ErrBusy) {
				code = http.StatusServiceUnavailable
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, http.StatusOK, request.Snapshot)
	})

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
