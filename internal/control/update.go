package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
)

const ReleaseOperationTimeout = 5 * time.Minute

type ReleasePrepareRequest struct {
	Release            release.Ref `json:"release"`
	ExpectedGeneration uint64      `json:"expected_generation"`
	ExpectedRevision   uint64      `json:"expected_revision"`
}

type ReleaseRollbackRequest struct {
	ExpectedGeneration uint64 `json:"expected_generation"`
	ExpectedRevision   uint64 `json:"expected_revision"`
}

type ReleaseCancelRequest struct {
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type ReleasePruneRequest struct {
	ExpectedGeneration uint64   `json:"expected_generation"`
	ReleaseDigests     []string `json:"release_digests"`
}

func (e *Engine) releaseRoutes(parent *http.ServeMux) {
	if e.releaseStore == nil {
		parent.HandleFunc("/v1/releases/", func(w http.ResponseWriter, r *http.Request) {
			localapi.Fail(w, http.StatusNotImplemented, errors.New("updates require a provisioned signed appliance runtime"))
		})
		return
	}
	mux := http.NewServeMux()
	parent.Handle("/v1/releases/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(ReleaseOperationTimeout)
		response := http.NewResponseController(w)
		if err := errors.Join(response.SetReadDeadline(deadline), response.SetWriteDeadline(deadline)); err != nil {
			releaseFailure(w, err)
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}))
	mux.HandleFunc("GET /v1/releases/status", func(w http.ResponseWriter, r *http.Request) {
		state, err := e.releaseStore.Status(r.Context())
		if err != nil {
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusOK, state)
	})
	// One disk-backed upload at a time; the release journal is not locked
	// while a remote sender supplies bytes.
	upload := make(chan struct{}, 1)
	parent.HandleFunc("POST /v1/releases/stage", func(w http.ResponseWriter, r *http.Request) {
		select {
		case upload <- struct{}{}:
			defer func() { <-upload }()
		default:
			releaseFailure(w, release.ErrBusy)
			return
		}
		file, err := unlinkedInput(e.store.Dir)
		if err != nil {
			releaseFailure(w, err)
			return
		}
		defer file.Close()
		response := http.NewResponseController(w)
		if err := response.SetWriteDeadline(time.Time{}); err != nil {
			releaseFailure(w, err)
			return
		}
		started := time.Now()
		body := http.MaxBytesReader(w, r.Body, e.releaseStore.BundleLimit())
		buffer := make([]byte, 64<<10)
		var received int64
		for {
			// Bound an idle sender, not the duration of a progressing upload.
			if err := response.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				releaseFailure(w, err)
				return
			}
			n, readErr := body.Read(buffer)
			if n != 0 {
				if _, err := file.Write(buffer[:n]); err != nil {
					releaseFailure(w, err)
					return
				}
				received += int64(n)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				releaseFailure(w, readErr)
				return
			}
		}
		slog.Info("release upload received", "bytes", received, "seconds", time.Since(started).Seconds())
		verified := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), ReleaseOperationTimeout)
		defer cancel()
		if err := response.SetWriteDeadline(time.Now().Add(ReleaseOperationTimeout)); err != nil {
			releaseFailure(w, err)
			return
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			releaseFailure(w, err)
			return
		}
		ref, err := e.releaseStore.Stage(ctx, file)
		if err != nil {
			slog.Warn("release staging failed", "error", err, "verify_seconds", time.Since(verified).Seconds())
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusCreated, ref)
	})
	mux.HandleFunc("POST /v1/releases/prepare", func(w http.ResponseWriter, r *http.Request) {
		var request ReleasePrepareRequest
		if err := localapi.Decode(w, r, &request); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		state, err := e.prepareRuntime(r.Context(), request.ExpectedGeneration, request.ExpectedRevision, &request.Release)
		if err != nil {
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /v1/releases/rollback", func(w http.ResponseWriter, r *http.Request) {
		var request ReleaseRollbackRequest
		if err := localapi.Decode(w, r, &request); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		state, err := e.prepareRuntime(r.Context(), request.ExpectedGeneration, request.ExpectedRevision, nil)
		if err != nil {
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /v1/releases/cancel", func(w http.ResponseWriter, r *http.Request) {
		var request ReleaseCancelRequest
		if err := localapi.Decode(w, r, &request); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		state, err := e.releaseStore.CancelPending(r.Context(), request.ExpectedGeneration)
		if err != nil {
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /v1/releases/prune", func(w http.ResponseWriter, r *http.Request) {
		var request ReleasePruneRequest
		if err := localapi.Decode(w, r, &request); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		result, err := e.releaseStore.Prune(r.Context(), request.ExpectedGeneration, request.ReleaseDigests)
		if err != nil {
			releaseFailure(w, err)
			return
		}
		localapi.JSON(w, http.StatusOK, result)
	})
}

// A nil target selects the previously confirmed runtime under the release lock.
// Both directions validate the current configuration before queuing a trial.
func (e *Engine) prepareRuntime(ctx context.Context, expectedGeneration, expectedRevision uint64, target *release.Ref) (release.State, error) {
	e.mu.Lock()
	if !e.state.Healthy || e.state.Confirmation != nil || expectedRevision != e.state.Revision {
		e.mu.Unlock()
		return release.State{}, ErrConflict
	}
	current := e.state.Committed
	var generation uint64
	if err := e.releaseStore.WithState(ctx, func(state release.State) error {
		if state.Generation != expectedGeneration {
			return release.ErrStale
		}
		if err := selectedConfiguration(state, e.releaseDigest, current); err != nil {
			return err
		}
		generation = state.ConfigHighWatermark + 1
		return nil
	}); err != nil {
		e.mu.Unlock()
		return release.State{}, err
	}
	snapshot, err := e.store.Snapshot(expectedRevision)
	if err != nil {
		e.mu.Unlock()
		return release.State{}, err
	}
	configuration, err := json.Marshal(current.Config)
	e.mu.Unlock()
	if err != nil {
		return release.State{}, err
	}
	selection := configref.Selection{Generation: generation, ExpectedRevision: expectedRevision, Snapshot: snapshot}
	file, err := unlinkedInput(e.store.Dir)
	if err != nil {
		return release.State{}, err
	}
	defer file.Close()
	if _, err := file.Write(configuration); err != nil {
		return release.State{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return release.State{}, err
	}
	check := func(call context.Context, state release.State, target *release.Image) error {
		if err := selectedConfiguration(state, e.releaseDigest, current); err != nil {
			return err
		}
		// PrepareChecked owns the release lock. All configuration writers need
		// that lock before writing; InspectStartup rejects a revision changed
		// between our capture and this transaction without blocking observations.
		if _, err := e.store.InspectStartup(selection); err != nil {
			return err
		}
		fallback := configref.Selection{Generation: generation + 1, ExpectedRevision: expectedRevision, Snapshot: snapshot,
			Recovery: &configref.Recovery{FailedGeneration: generation, FailedSnapshot: snapshot}}
		if _, err := e.store.InspectStartup(fallback); err != nil {
			return err
		}
		command := exec.CommandContext(call, "/proc/self/fd/3", "config", "validate", "/proc/self/fd/4")
		command.ExtraFiles = []*os.File{target.Entrypoint(), file}
		var diagnostic bytes.Buffer
		command.Stderr = &diagnostic
		if err := command.Run(); err != nil {
			return fmt.Errorf("target configuration validation: %w: %s", err, diagnostic.String())
		}
		return nil
	}
	options := release.PrepareOptions{Startup: &selection, KnownGood: &snapshot}
	if target == nil {
		return e.releaseStore.PrepareRollbackChecked(ctx, expectedGeneration, options, check)
	}
	return e.releaseStore.PrepareChecked(ctx, *target, expectedGeneration, options, check)
}

// The filesystem supplies storage without retaining an orphan after a crash.
func unlinkedInput(dir string) (*os.File, error) {
	file, err := os.CreateTemp(dir, ".release-input-")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func releaseFailure(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	var oversized *http.MaxBytesError
	switch {
	case errors.As(err, &oversized):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, release.ErrStale), errors.Is(err, release.ErrDowngrade), errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, release.ErrBusy), errors.Is(err, release.ErrCapacity), errors.Is(err, release.ErrUncertain):
		status = http.StatusServiceUnavailable
	}
	localapi.Fail(w, status, err)
}
