//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type restartRequest struct {
	name  string
	reply chan error
}

type api struct {
	mu       sync.RWMutex
	status   Status
	restarts chan restartRequest
	ctx      context.Context
	server   *http.Server
	listener *net.UnixListener
	lock     *os.File
	path     string
}

func openAPI(ctx context.Context, dir string, initial Status) (*api, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		return nil, errors.New("runtime directory must be an absolute non-root path")
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("runtime directory must not be a symlink")
	}
	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		return nil, err
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("runtime directory is not owned by this supervisor's uid")
	}
	if err := os.Chmod(dir, 0750); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(dir, "supervisor.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open supervisor lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), "supervisor.lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("runtime directory is already supervised: %w", err)
	}
	path := filepath.Join(dir, SocketName)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			_ = lock.Close()
			return nil, fmt.Errorf("refusing to replace non-socket %s", path)
		}
		if err := os.Remove(path); err != nil {
			_ = lock.Close()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		_ = lock.Close()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		_ = lock.Close()
		return nil, err
	}
	a := &api{status: initial, restarts: make(chan restartRequest), ctx: ctx, listener: listener, lock: lock, path: path}
	a.server = &http.Server{
		Handler: a, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192,
	}
	return a, nil
}

func (a *api) close() {
	_ = a.server.Close()
	_ = a.listener.Close()
	_ = os.Remove(a.path)
	_ = a.lock.Close()
}

func (a *api) publish(s Status) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/status" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		s := a.status
		a.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(s)
		return
	}
	const prefix, suffix = "/v1/modules/", "/restart"
	if strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, suffix) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, `{"error":"invalid module name"}`, http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		req := restartRequest{name: name, reply: make(chan error, 1)}
		select {
		case a.restarts <- req:
		case <-a.ctx.Done():
			http.Error(w, `{"error":"supervisor is stopping"}`, http.StatusServiceUnavailable)
			return
		case <-r.Context().Done():
			return
		}
		select {
		case err := <-req.reply:
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("{\"accepted\":true}\n"))
		case <-a.ctx.Done():
			http.Error(w, `{"error":"supervisor is stopping"}`, http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
		return
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}
