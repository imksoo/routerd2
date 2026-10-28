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
	"syscall"
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

type replicationUpdate struct {
	revision uint64
	data     []byte
}

type replicationOutcome struct {
	revision uint64
	err      error
}

type reconciliation struct {
	journal journal
	err     error
}

func (r *replication) poll(ctx context.Context, cfg Config, results chan<- reconciliation) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		remote, err := r.pull(ctx, cfg)
		select {
		case results <- reconciliation{journal: remote, err: err}:
		case <-ctx.Done():
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// Only transport unavailability permits starting from the local journal.
// Authentication, allocation-domain and journal errors remain fatal.
func peerUnavailable(err error) bool {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return false
	}
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ECONNRESET) || errors.As(err, &networkError) && networkError.Timeout()
}

// push owns the latest unsent durable journal and retries while the peer is
// unavailable. The DHCP loop never waits on the network to answer a client.
func (r *replication) push(ctx context.Context, updates <-chan replicationUpdate, outcomes chan replicationOutcome) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var pending replicationUpdate
	for {
		select {
		case <-ctx.Done():
			return
		case pending = <-updates:
		case <-ticker.C:
		}
		if pending.data == nil {
			continue
		}
		_, err := r.exchange(ctx, http.MethodPut, pending.data)
		outcome := replicationOutcome{revision: pending.revision, err: err}
		select {
		case outcomes <- outcome:
		default:
			select {
			case <-outcomes:
			default:
			}
			outcomes <- outcome
		}
		if err == nil {
			pending = replicationUpdate{}
		}
	}
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
			if err == nil && item.state.Owner != cfg.Replication.PeerAddress.String() {
				err = errors.New("DHCP journal does not belong to the authenticated peer")
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
		if !sameJournal(e.journal(), next) {
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

func sameJournal(a, b journal) bool {
	// Both sides may retransmit after an ACK was lost. Compare facts independent
	// of map traversal order, while retaining the original write time.
	sortJournal(a)
	sortJournal(b)
	return a.Owner == b.Owner && a.WrittenAt.Equal(b.WrittenAt) && slices.Equal(a.Leases, b.Leases) && slices.Equal(a.Quarantines, b.Quarantines)
}

// containsJournal prevents a newer revision from discarding a still-held local
// binding or quarantine from a disconnected owner.
func containsJournal(next, previous journal, now time.Time) bool {
	leases := make(map[string]storedLease, len(next.Leases))
	for _, lease := range next.Leases {
		leases[leaseKey(lease.Lease)] = lease
	}
	for _, old := range previous.Leases {
		if !old.HeldUntil.After(now) {
			continue
		}
		current, ok := leases[leaseKey(old.Lease)]
		if !ok || current.IPAddress != old.IPAddress || current.ExpiresAt.Before(old.ExpiresAt) || current.HeldUntil.Before(old.HeldUntil) || old.Expired && !current.Expired {
			return false
		}
	}
	quarantines := make(map[string]time.Time, len(next.Quarantines))
	for _, q := range next.Quarantines {
		quarantines[q.IPAddress] = q.Until
	}
	for _, old := range previous.Quarantines {
		if old.Until.After(now) && quarantines[old.IPAddress].Before(old.Until) {
			return false
		}
	}
	return true
}

// mergeJournal retains unmatched bindings as non-authoritative holds. A client
// can renew its own address, but an unseen release cannot become a DNS claim or
// a free address. Ambiguous addresses or identities require operator resolution.
func mergeJournal(a, b journal, now time.Time) (journal, error) {
	leases := make(map[string]storedLease)
	addresses := make(map[string]string)
	seen := make(map[string]int)
	live := make(map[string]int)
	for _, source := range []journal{a, b} {
		for _, lease := range source.Leases {
			if !lease.HeldUntil.After(now) {
				continue
			}
			key := leaseKey(lease.Lease)
			seen[key]++
			if !lease.Expired {
				live[key]++
			}
			if other, ok := addresses[lease.IPAddress]; ok && other != key {
				return journal{}, errors.New("conflicting DHCP lease addresses require reconciliation")
			}
			addresses[lease.IPAddress] = key
			if old, ok := leases[key]; ok {
				if old.IPAddress != lease.IPAddress {
					return journal{}, errors.New("conflicting DHCP client addresses require reconciliation")
				}
				heldUntil := old.HeldUntil
				if lease.HeldUntil.After(heldUntil) {
					heldUntil = lease.HeldUntil
				}
				if lease.ExpiresAt.After(old.ExpiresAt) || lease.ExpiresAt.Equal(old.ExpiresAt) && lease.HeldUntil.After(old.HeldUntil) {
					old = lease
				}
				old.HeldUntil = heldUntil
				leases[key] = old
			} else {
				leases[key] = lease
			}
		}
	}
	quarantines := make(map[string]quarantine)
	for _, source := range []journal{a, b} {
		for _, q := range source.Quarantines {
			if !q.Until.After(now) {
				continue
			}
			if _, occupied := addresses[q.IPAddress]; occupied {
				return journal{}, errors.New("DHCP quarantine conflicts with a retained lease")
			}
			if old, ok := quarantines[q.IPAddress]; !ok || q.Until.After(old.Until) {
				quarantines[q.IPAddress] = q
			}
		}
	}
	out := journal{Version: journalVersion, Resource: a.Resource, Interface: a.Interface, Owner: a.Owner, Revision: max(a.Revision, b.Revision), WrittenAt: now}
	for key, lease := range leases {
		lease.Expired = seen[key] < 2 || live[key] < 2
		out.Leases = append(out.Leases, lease)
	}
	for _, q := range quarantines {
		out.Quarantines = append(out.Quarantines, q)
	}
	return out, nil
}

func reconcileJournals(local, peer journal, now time.Time, limit int) (journal, error) {
	var result journal
	var err error
	switch {
	case peer.Revision > local.Revision && containsJournal(peer, local, now):
		result = peer
	case local.Revision > peer.Revision && containsJournal(local, peer, now):
		result = local
	case local.Revision == 0 && peer.Revision == 0:
		result = local
	case local.Revision == peer.Revision && sameJournal(local, peer):
		result = local
	default:
		result, err = mergeJournal(local, peer, now)
	}
	if err != nil {
		return journal{}, err
	}
	if len(result.Leases) > limit || len(result.Quarantines) > limit {
		return journal{}, errors.New("reconciled DHCP journal exceeds the configured lease limit")
	}
	return result, nil
}
