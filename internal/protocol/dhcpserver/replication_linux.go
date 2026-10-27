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
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/peertls"
)

const replicationTimeout = 2 * time.Second

type replicationRequest struct {
	write bool
	state journal
	done  chan replicationResult
}
type replicationResult struct {
	data []byte
	err  error
}

// The DHCP loop alone owns the journal and applies peer writes. HTTP handlers
// authenticate/decode the boundary and wait for that owner's durable result.
type replication struct {
	server   *http.Server
	client   *http.Client
	peer     string
	domain   string
	requests chan replicationRequest
	done     chan struct{}
	err      error // written before done is closed
}

func startReplication(ctx context.Context, cfg Config) (*replication, error) {
	c := cfg.Replication
	serverTLS, clientTLS, err := peertls.Load(ctx, *c)
	if err != nil {
		return nil, err
	}
	// Interface names and local file paths are host-specific. The allocation
	// domain must agree before either side accepts the other's semantic state.
	body, err := json.Marshal(struct {
		Resource      string
		ServerAddress string
		Primary       Scope
		Scopes        []Scope       `json:"scopes,omitempty"`
		Reservations  []Reservation `json:"reservations,omitempty"`
		StickyDays    int
	}{cfg.Resource, cfg.ServerAddress, cfg.Primary, cfg.Scopes, cfg.Reservations, cfg.StickyHoldDays})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	endpoint := (&url.URL{Scheme: "https", Host: c.PeerAddress.String(), Path: "/v1/leases"}).String()
	r := &replication{domain: hex.EncodeToString(sum[:]), peer: endpoint, requests: make(chan replicationRequest, 1), done: make(chan struct{})}
	transport := &http.Transport{TLSClientConfig: clientTLS}
	r.client = &http.Client{Transport: transport, Timeout: replicationTimeout}
	r.server = &http.Server{Handler: r.handler(cfg), ReadHeaderTimeout: time.Second, ReadTimeout: replicationTimeout, WriteTimeout: 2 * replicationTimeout, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.ListenAddress.String())
	if err != nil {
		return nil, err
	}
	secured := tls.NewListener(listener, serverTLS)
	go func() { r.err = r.server.Serve(secured); close(r.done) }()
	return r, nil
}

func (r *replication) close() {
	r.server.Close()
	<-r.done
	r.client.CloseIdleConnections()
}

func (r *replication) handler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/leases" {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodPut && req.Header.Get("X-Routerd-DHCP-Domain") != r.domain {
			http.Error(w, "different DHCP allocation domain", http.StatusConflict)
			return
		}
		if !cfg.Standby {
			http.Error(w, "DHCP allocator is serving", http.StatusConflict)
			return
		}
		item := replicationRequest{done: make(chan replicationResult, 1)}
		switch req.Method {
		case http.MethodGet:
		case http.MethodPut:
			item.write = true
			d := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxJournalBytes))
			d.DisallowUnknownFields()
			err := d.Decode(&item.state)
			if err == nil {
				var trailing json.RawMessage
				if d.Decode(&trailing) != io.EOF {
					err = errors.New("trailing DHCP replication state")
				}
			}
			// Journal file ownership is local; the authenticated allocation-domain
			// fingerprint covers the shared pools, reservations and sticky policy.
			item.state.Interface = cfg.Interface
			if err == nil {
				err = validateJournal(cfg, item.state, time.Now().UTC())
			}
			if err != nil || len(item.state.Leases) > cfg.MaxLeases || len(item.state.Quarantines) > cfg.MaxLeases {
				http.Error(w, "invalid DHCP replication state", http.StatusBadRequest)
				return
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), replicationTimeout)
		defer cancel()
		select {
		case r.requests <- item:
		case <-ctx.Done():
			return
		}
		select {
		case result := <-item.done:
			if result.err != nil {
				http.Error(w, result.err.Error(), http.StatusConflict)
				return
			}
			w.Header().Set("X-Routerd-DHCP-Domain", r.domain)
			w.Header().Set("Content-Type", "application/json")
			if item.write {
				w.WriteHeader(http.StatusNoContent)
			} else {
				_, _ = w.Write(result.data)
			}
		case <-ctx.Done():
		}
	})
}

func (r *replication) exchange(ctx context.Context, method string, data []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.peer, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Routerd-DHCP-Domain", r.domain)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Routerd-DHCP-Domain") != r.domain || resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("DHCP peer did not acknowledge standby state: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJournalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJournalBytes {
		return nil, errors.New("DHCP peer snapshot exceeds size bound")
	}
	return body, nil
}

func (r *replication) pull(ctx context.Context, cfg Config) (journal, error) {
	data, err := r.exchange(ctx, http.MethodGet, nil)
	if err != nil {
		return journal{}, err
	}
	var j journal
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return journal{}, err
	}
	var trailing json.RawMessage
	if d.Decode(&trailing) != io.EOF {
		return journal{}, errors.New("trailing DHCP peer snapshot")
	}
	j.Interface = cfg.Interface
	return j, validateJournal(cfg, j, time.Now().UTC())
}

func applyReplica(e *engine, store *journalStore, next journal) error {
	if next.Revision < e.revision {
		return errors.New("stale DHCP replication revision")
	}
	if next.Revision == e.revision {
		current := e.journal()
		// Both sides may retransmit after an ACK was lost. Compare canonical
		// facts, independent of map traversal order, before accepting that retry.
		sortJournal(current)
		sortJournal(next)
		if !current.WrittenAt.Equal(next.WrittenAt) || !slices.Equal(current.Leases, next.Leases) || !slices.Equal(current.Quarantines, next.Quarantines) {
			return errors.New("conflicting DHCP replication revision")
		}
		return nil
	}
	if err := store.Save(next); err != nil {
		return err
	}
	e.restore(next)
	return nil
}
