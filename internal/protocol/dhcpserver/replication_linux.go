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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/peertls"
)

const replicaTimeout = 2 * time.Second
const maxLeaseFile = 8 << 20

// The lease file remains dnsmasq's format and allocation authority. The owner
// marker records only who may continue allocating during a sync-link outage.
type leaseTransfer struct {
	Owner   string         `json:"owner"`
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
	owner, err := os.ReadFile(cfg.StatePath + ".owner")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	state.Owner = string(owner)
	state.History, err = readHistory(ctx, cfg, time.Now())
	if err == nil {
		state.History = mergeHistory(state.History, expiredHistory(cfg, state.Leases, time.Now()))
	}
	return state, err
}

func saveTransfer(ctx context.Context, cfg Config, state leaseTransfer) error {
	current, err := readTransfer(ctx, cfg)
	if err != nil {
		return err
	}
	if current.Owner == state.Owner && current.Leases == state.Leases && slices.Equal(current.History, state.History) {
		return nil
	}
	// A crash between the two files must leave ownership unknown, rather than
	// allowing allocation from a different owner's incomplete transfer.
	if err := replaceFile(cfg.StatePath+".owner", nil); err != nil {
		return err
	}
	if err := replaceFile(cfg.StatePath, []byte(state.Leases)); err != nil {
		return err
	}
	if err := saveHistory(ctx, cfg, state.History); err != nil {
		return err
	}
	return replaceFile(cfg.StatePath+".owner", []byte(state.Owner))
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
		if !cfg.Standby || req.Header.Get("X-Routerd-DHCP-Domain") != r.domain {
			http.Error(w, "not standby or different allocation domain", http.StatusConflict)
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
			if err == nil && incoming.Owner != cfg.Replication.PeerAddress.String() {
				err = errors.New("lease owner differs from authenticated peer")
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
	records := map[string][]string{}
	clients := map[string]string{}
	for _, data := range []string{a, b} {
		for _, line := range strings.Split(data, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "agent-info" || fields[0] == "vendorclass" {
				continue
			}
			if len(fields) != 5 {
				return "", errors.New("incomplete dnsmasq lease record")
			}
			if removed[[2]string{fields[1], fields[2]}] {
				continue
			}
			expires, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil || expires == 0 {
				return "", errors.New("DHCP handoff requires finite lease expiries")
			}
			if expires <= now.Unix() {
				continue
			}
			if _, err := net.ParseMAC(fields[1]); err != nil {
				return "", err
			}
			if ip := net.ParseIP(fields[2]); ip == nil || ip.To4() == nil {
				return "", errors.New("invalid DHCP lease address")
			}
			key := fields[4]
			if key == "*" {
				key = fields[1]
			}
			if previous := clients[key]; previous != "" && previous != fields[2] {
				return "", errors.New("conflicting DHCP client addresses")
			}
			clients[key] = fields[2]
			if old, ok := records[fields[2]]; ok {
				if old[1] != fields[1] || old[4] != fields[4] {
					return "", errors.New("conflicting DHCP address owners")
				}
				oldExpiry, _ := strconv.ParseInt(old[0], 10, 64)
				if oldExpiry > expires {
					continue
				}
			}
			records[fields[2]] = fields
		}
	}
	addresses := make([]string, 0, len(records))
	for address := range records {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	var lines strings.Builder
	for _, address := range addresses {
		lines.WriteString(strings.Join(records[address], " "))
		lines.WriteByte('\n')
	}
	return lines.String(), nil
}
