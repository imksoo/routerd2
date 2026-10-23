// SPDX-License-Identifier: BSD-3-Clause
package bgp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"golang.org/x/sys/unix"
)

func controlHandler(statePath string, paths pathServer, appliedStateMu sync.Locker, monitor *observation) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/applied", func(w http.ResponseWriter, r *http.Request) {
		appliedStateMu.Lock()
		defer appliedStateMu.Unlock()
		switch r.Method {
		case http.MethodGet:
			config, _, err := readApplied(statePath)
			if err != nil {
				localapi.Fail(w, 500, err)
				return
			}
			writeJSON(w, config)
		case http.MethodPut:
			var config bgpdaemon.AppliedConfig
			if err := localapi.Decode(w, r, &config); err != nil {
				localapi.Fail(w, 400, err)
				return
			}
			if err := bgpconfig.ValidateRuntime(config); err != nil {
				localapi.Fail(w, 400, err)
				return
			}
			if err := writeApplied(statePath, config); err != nil {
				localapi.Fail(w, 500, err)
				return
			}
			writeJSON(w, bgpdaemon.Normalize(config))
		default:
			localapi.Fail(w, 405, errors.New("method not allowed"))
		}
	})
	mux.HandleFunc("/v1/paths", func(w http.ResponseWriter, r *http.Request) {
		appliedStateMu.Lock()
		defer appliedStateMu.Unlock()
		switch r.Method {
		case http.MethodGet:
			config, _, err := readApplied(statePath)
			if err != nil {
				localapi.Fail(w, 500, err)
				return
			}
			source := strings.TrimSpace(r.URL.Query().Get("source"))
			out := config.Paths
			if source != "" {
				out = nil
				for _, path := range config.Paths {
					if path.Source == source {
						out = append(out, path)
					}
				}
			}
			writeJSON(w, out)
		case http.MethodPost:
			var path bgpdaemon.AppliedPath
			if err := localapi.Decode(w, r, &path); err != nil {
				localapi.Fail(w, 400, err)
				return
			}
			applied, updated, err := upsertDynamicPath(r.Context(), paths, statePath, path)
			if err != nil {
				signalTransactionFailure(monitor, err)
				localapi.Fail(w, httpStatusForPathError(err), err)
				return
			}
			if updated == nil {
				writeJSON(w, applied)
			} else {
				writeJSON(w, updated)
			}
		case http.MethodDelete:
			path := bgpdaemon.AppliedPath{Source: strings.TrimSpace(r.URL.Query().Get("source")), Prefix: strings.TrimSpace(r.URL.Query().Get("prefix"))}
			if path.Source == "" && path.Prefix == "" {
				if err := localapi.Decode(w, r, &path); err != nil {
					localapi.Fail(w, 400, err)
					return
				}
			}
			config, err := deleteDynamicPath(r.Context(), paths, statePath, path)
			if err != nil {
				signalTransactionFailure(monitor, err)
				localapi.Fail(w, httpStatusForPathError(err), err)
				return
			}
			writeJSON(w, config)
		default:
			localapi.Fail(w, 405, errors.New("method not allowed"))
		}
	})
	mux.HandleFunc("/v1/observed-paths", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			localapi.Fail(w, 405, errors.New("method not allowed"))
			return
		}
		observer, ok := paths.(observedPathServer)
		if !ok {
			localapi.Fail(w, 503, errors.New("live BGP RIB observation is unavailable"))
			return
		}
		observed, err := observer.ListObservedPaths(r.Context())
		if err != nil {
			localapi.Fail(w, 500, err)
			return
		}
		writeJSON(w, observed)
	})
	if monitor != nil {
		mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				localapi.Fail(w, 405, errors.New("method not allowed"))
				return
			}
			monitor.mu.RLock()
			latest := cloneSnapshot(monitor.latest)
			monitor.mu.RUnlock()
			writeJSON(w, latest)
		})
		mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				localapi.Fail(w, 405, errors.New("method not allowed"))
				return
			}
			after := uint64(0)
			if value := r.URL.Query().Get("after"); value != "" {
				var err error
				after, err = strconv.ParseUint(value, 10, 64)
				if err != nil {
					localapi.Fail(w, 400, errors.New("invalid event cursor"))
					return
				}
			}
			monitor.mu.RLock()
			defer monitor.mu.RUnlock()
			out := make([]Event, 0)
			epoch := r.URL.Query().Get("epoch")
			gap := epoch != "" && epoch != monitor.latest.Epoch
			if gap {
				after = 0
			}
			if len(monitor.events) > 0 && after != 0 && after < monitor.events[0].Sequence-1 {
				gap = true
			}
			for _, event := range monitor.events {
				if event.Sequence > after {
					out = append(out, event)
				}
			}
			writeJSON(w, struct {
				Epoch  string  `json:"epoch"`
				Cursor uint64  `json:"cursor"`
				Gap    bool    `json:"gap"`
				Events []Event `json:"events"`
			}{monitor.latest.Epoch, monitor.latest.Sequence, gap, out})
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The upstream gRPC server creates its own listener asynchronously. A private
// parent directory prevents access during the short listen-to-chmod interval.
// Never change permissions on an existing shared directory to achieve this.
func privateSocketPath(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0077 != 0 {
		return errors.New("BGP API sockets require a private (0700) parent directory")
	}
	fi, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return errors.New("refuse to replace a non-socket BGP API path")
	}
	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = c.Close()
		return errors.New("BGP API socket is already active")
	}
	if !errors.Is(err, unix.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot establish stale BGP API socket ownership: %w", err)
	}
	return os.Remove(path)
}

func listenPrivate(path string) (net.Listener, error) {
	if err := privateSocketPath(path); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func httpServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192}
}

// Compatibility harness helpers retained for the upstream transaction tests.
func serveControlSocket(socketPath, statePath string, paths pathServer) (*http.Server, error) {
	return serveControlSocketWithStateLock(socketPath, statePath, paths, &sync.Mutex{})
}
func serveControlSocketWithStateLock(socketPath, statePath string, paths pathServer, lock sync.Locker) (*http.Server, error) {
	l, err := listenPrivate(socketPath)
	if err != nil {
		return nil, err
	}
	s := httpServer(controlHandler(statePath, paths, lock, nil))
	go func() { _ = s.Serve(l) }()
	return s, nil
}
