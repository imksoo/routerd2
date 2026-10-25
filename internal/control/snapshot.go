package control

import (
	"errors"
	"net/http"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
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

func (e *Engine) snapshotRoutes(mux *http.ServeMux) {
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
