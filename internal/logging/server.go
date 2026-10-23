package logging

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
)

// Run owns the logging worker's private socket and disk queue. The worker's own
// diagnostics should go directly to its console, not back into its ingest API.
func Run(ctx context.Context, options Options, ready func()) error {
	if !filepath.IsAbs(options.RuntimeDir) || filepath.Clean(options.RuntimeDir) == "/" {
		return errors.New("logging runtime directory must be an absolute non-root path")
	}
	store, err := Open(options)
	if err != nil {
		return err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.Close(stop)
	}()
	if options.CaptureKernel {
		kernelContext, cancelKernel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			captureKernel(kernelContext, store)
		}()
		defer func() {
			cancelKernel()
			<-done // The native reader is nonblocking and waits at most 200 ms.
		}()
	}
	return localapi.Serve(ctx, filepath.Join(options.RuntimeDir, SocketName), NewAPI(store), ready)
}

// NewAPI exposes snapshots, not a streaming response. Slow readers retain no
// store lock and have a separate concurrency budget from producers.
func NewAPI(store *Store) http.Handler {
	queries := make(chan struct{}, 8)
	ingests := make(chan struct{}, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			localapi.Fail(w, http.StatusMethodNotAllowed, errors.New("GET required"))
			return
		}
		localapi.JSON(w, http.StatusOK, store.Status())
	})
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			select {
			case ingests <- struct{}{}:
				defer func() { <-ingests }()
			default:
				localapi.Fail(w, http.StatusTooManyRequests, errors.New("log ingestion is busy"))
				return
			}
			var request IngestRequest
			// This tighter limit composes with the shared strict JSON decoder.
			r.Body = http.MaxBytesReader(w, r.Body, MaxBatch*MaxRecordBytes+1024)
			if err := localapi.Decode(w, r, &request); err != nil {
				localapi.Fail(w, http.StatusBadRequest, err)
				return
			}
			result, err := store.Append(request.Entries)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, ErrClosed) {
					status = http.StatusServiceUnavailable
				}
				localapi.Fail(w, status, err)
				return
			}
			localapi.JSON(w, http.StatusAccepted, result)
		case http.MethodGet:
			select {
			case queries <- struct{}{}:
				defer func() { <-queries }()
			default:
				localapi.Fail(w, http.StatusTooManyRequests, errors.New("log queries are busy"))
				return
			}
			q, err := parseQuery(r)
			if err != nil {
				localapi.Fail(w, http.StatusBadRequest, err)
				return
			}
			result, err := store.Query(q)
			if err != nil {
				localapi.Fail(w, http.StatusBadRequest, err)
				return
			}
			localapi.JSON(w, http.StatusOK, result)
		default:
			localapi.Fail(w, http.StatusMethodNotAllowed, errors.New("GET or POST required"))
		}
	})
	return mux
}

func parseQuery(r *http.Request) (Query, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if len(entries) != 1 {
			return Query{}, errors.New("duplicate log query parameter")
		}
		switch name {
		case "epoch", "after", "limit", "module", "level", "tail":
		default:
			return Query{}, errors.New("unknown log query parameter")
		}
	}
	q := Query{Module: values.Get("module"), MinLevel: strings.ToUpper(values.Get("level")), Cursor: Cursor{Epoch: values.Get("epoch")}}
	var err error
	if value := values.Get("after"); value != "" {
		q.Cursor.Sequence, err = strconv.ParseUint(value, 10, 64)
		if err != nil || q.Cursor.Epoch == "" {
			return q, errors.New("after requires an epoch and a nonnegative sequence")
		}
	}
	if q.Cursor.Epoch != "" && !epochName.MatchString(q.Cursor.Epoch) {
		return q, errors.New("invalid cursor epoch")
	}
	if value := values.Get("limit"); value != "" {
		q.Limit, err = strconv.Atoi(value)
		if err != nil || q.Limit < 1 || q.Limit > MaxQuery {
			return q, errors.New("limit must be 1..200")
		}
	}
	q.Tail = values.Get("after") == "" && q.Cursor.Epoch == ""
	if value := values.Get("tail"); value != "" {
		if value != "true" && value != "false" {
			return q, errors.New("tail must be true or false")
		}
		q.Tail = value == "true"
	}
	return q, nil
}
