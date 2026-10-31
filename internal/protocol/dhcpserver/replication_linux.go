// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/dhcplease"
	"github.com/imksoo/routerd2/internal/peertls"
)

const replicaTimeout = 2 * time.Second
const maxLeaseFile = 8 << 20

var errPeerActive = errors.New("DHCP peer is also serving this allocation domain")

// VRRP selects serving authority. This transfers dnsmasq allocation facts
// and sticky history; peer availability never elects a second owner.
type leaseTransfer struct {
	Leases  string         `json:"leases"`
	History []leaseHistory `json:"history,omitempty"`
}

type replication struct {
	server       *http.Server
	client       *http.Client
	peer, domain string
	done         chan struct{}
	err          error      // written before done closes
	mu           sync.Mutex // serializes standby file replacement and reads
	cancel       context.CancelFunc
}

func replaceFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".lease-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readTransfer(ctx context.Context, cfg Config) (leaseTransfer, error) {
	var state leaseTransfer
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	state.Leases = string(data)
	state.History, err = readHistory(ctx, cfg, time.Now())
	if err == nil {
		state.History = mergeHistory(state.History, expiredHistory(cfg, state.Leases, time.Now()))
	}
	return state, err
}

func saveTransfer(ctx context.Context, cfg Config, state leaseTransfer) error {
	// A standby can retain a pre-migration lease until this authenticated
	// exchange. This write runs only before serving or after the active engine
	// has stopped, so the persisted file remains the sole allocation state.
	state.Leases, _ = pruneScopeLeaseData(cfg, state.Leases)
	current, err := readTransfer(ctx, cfg)
	if err != nil {
		return err
	}
	if current.Leases == state.Leases && slices.Equal(current.History, state.History) {
		return nil
	}
	// Persist removals and holds before the lease file. On an interrupted
	// transfer, startup can exclude old bindings using the retained history.
	if err := saveHistory(ctx, cfg, state.History); err != nil {
		return err
	}
	return replaceFile(cfg.StatePath, []byte(state.Leases))
}

// Like routerd's file-sync path, this transfers the engine's own lease file.
// Unlike a shell/rsync job, the existing peer TLS contract authenticates it.
func startReplication(ctx context.Context, cfg Config) (*replication, error) {
	serverTLS, clientTLS, err := peertls.Load(ctx, *cfg.Replication)
	if err != nil {
		return nil, err
	}
	shared, err := json.Marshal(struct {
		Resource, Server string
		Primary          Scope
		Scopes           []Scope
		Reservations     []Reservation
		StickyHoldDays   int
	}{cfg.Resource, cfg.ServerAddress, cfg.Primary, cfg.Scopes, cfg.Reservations, cfg.StickyHoldDays})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(shared)
	ctx, cancel := context.WithCancel(ctx)
	r := &replication{domain: hex.EncodeToString(sum[:]), peer: (&url.URL{Scheme: "https", Host: cfg.Replication.PeerAddress.String(), Path: "/leases"}).String(), done: make(chan struct{}), cancel: cancel}
	r.client = &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: replicaTimeout}
	r.server = &http.Server{ReadHeaderTimeout: time.Second, ReadTimeout: replicaTimeout, WriteTimeout: 2 * replicaTimeout, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/leases" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("X-Routerd-DHCP-Domain", r.domain)
		if req.Header.Get("X-Routerd-DHCP-Domain") != r.domain {
			http.Error(w, "different allocation domain", http.StatusPreconditionFailed)
			return
		}
		if !cfg.Standby {
			http.Error(w, "peer is serving", http.StatusConflict)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if ctx.Err() != nil {
			http.Error(w, "DHCP replication stopped", http.StatusServiceUnavailable)
			return
		}
		state, err := readTransfer(req.Context(), cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch req.Method {
		case http.MethodGet:
			w.Header().Set("X-Routerd-DHCP-Domain", r.domain)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		case http.MethodPut:
			var incoming leaseTransfer
			decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxLeaseFile))
			err := decoder.Decode(&incoming)
			if err == nil {
				err = normalizeHistory(incoming.History)
			}
			if err == nil {
				incoming.History = mergeHistory(state.History, incoming.History)
				incoming.Leases, err = mergeLeases(state.Leases, incoming.Leases, incoming.History, time.Now())
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := saveTransfer(req.Context(), cfg, incoming); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Routerd-DHCP-Domain", r.domain)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Replication.ListenAddress.String())
	if err != nil {
		cancel()
		r.client.CloseIdleConnections()
		return nil, err
	}
	go func() { r.err = r.server.Serve(tls.NewListener(listener, serverTLS)); close(r.done) }()
	return r, nil
}

func (r *replication) close() {
	// Ownership is ending. Cancel peer requests now; local atomic state and
	// the next authenticated exchange handle recovery after a lost reply.
	r.cancel()
	r.server.Close()
	<-r.done
	// Closing sockets interrupts body reads; let an admitted atomic write
	// finish before the worker releases its file lock. Later handlers cannot
	// start a write after cancellation.
	r.mu.Lock()
	r.mu.Unlock()
	r.client.CloseIdleConnections()
}

func (r *replication) exchange(ctx context.Context, method string, state leaseTransfer) (leaseTransfer, error) {
	var result leaseTransfer
	data, err := json.Marshal(state)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.peer, bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	req.Header.Set("X-Routerd-DHCP-Domain", r.domain)
	resp, err := r.client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Routerd-DHCP-Domain") == r.domain && resp.StatusCode == http.StatusConflict {
		return result, errPeerActive
	}
	if resp.Header.Get("X-Routerd-DHCP-Domain") != r.domain || resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return result, fmt.Errorf("DHCP standby: HTTP %d", resp.StatusCode)
	}
	if method == http.MethodPut {
		return result, nil
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxLeaseFile)).Decode(&result)
	if err == nil {
		err = normalizeHistory(result.History)
	}
	return result, err
}

// Retain unexpired, unmatched bindings until their original expiry. A file
// copy concurrent with dnsmasq's rewrite must not free an offline client's IP.
func mergeLeases(a, b string, history []leaseHistory, now time.Time) (string, error) {
	removed := map[[2]string]bool{}
	for _, record := range history {
		if record.RemovedAt.After(record.ContactAt) {
			removed[[2]string{record.MAC, record.IP}] = true
		}
	}
	return dhcplease.Merge(a, b, removed, now)
}
