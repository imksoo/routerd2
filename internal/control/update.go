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

type ReleaseCancelRequest struct {
	ExpectedGeneration uint64 `json:"expected_generation"`
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
		var state release.State
		err := e.releaseStore.WithState(r.Context(), func(current release.State) error {
			state = current
			return nil
		})
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
		state, err := e.prepareRelease(r.Context(), request)
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
}

func (e *Engine) prepareRelease(ctx context.Context, request ReleasePrepareRequest) (release.State, error) {
	e.mu.Lock()
	if !e.state.Healthy || e.state.Confirmation != nil || request.ExpectedRevision != e.state.Revision {
		e.mu.Unlock()
		return release.State{}, ErrConflict
	}
	current := e.state.Committed
	var generation uint64
	if err := e.releaseStore.WithState(ctx, func(state release.State) error {
		if state.Generation != request.ExpectedGeneration {
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
	snapshot, err := e.store.Snapshot(request.ExpectedRevision)
	if err != nil {
		e.mu.Unlock()
		return release.State{}, err
	}
	configuration, err := json.Marshal(current.Config)
	e.mu.Unlock()
	if err != nil {
		return release.State{}, err
	}
	selection := configref.Selection{Generation: generation, ExpectedRevision: request.ExpectedRevision, Snapshot: snapshot}
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
		fallback := configref.Selection{Generation: generation + 1, ExpectedRevision: request.ExpectedRevision, Snapshot: snapshot,
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
	return e.releaseStore.PrepareChecked(ctx, request.Release, request.ExpectedGeneration,
		release.PrepareOptions{Startup: &selection, KnownGood: &snapshot}, check)
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
