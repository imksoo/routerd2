//go:build linux

package dhcp6

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
	"reflect"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
)

const replicaTimeout = 2 * time.Second
const maxCheckpointBytes = 2 << 20

type replicaReply struct {
	State      checkpoint `json:"state"`
	ClockNanos uint64     `json:"clock_nanos"`
}
type replicaResult struct {
	data []byte
	err  error
}
type replication struct {
	server             *http.Server
	client             *http.Client
	peer, domain, duid string
	requests           chan chan replicaResult
	done               chan struct{}
	err                error // written before done closes
}

func startReplication(ctx context.Context, cfg dhcp6config.Config) (*replication, error) {
	c := cfg.Replication
	serverTLS, clientTLS, err := peertls.Load(ctx, *c)
	if err != nil {
		return nil, err
	}
	duid, err := dhcp6config.ParseDUID(cfg.ClientDUID)
	if err != nil {
		return nil, err
	}
	duidText := hex.EncodeToString(duid)
	domain, err := json.Marshal(struct {
		Resource, DUID  string
		IAID            uint32
		Address, Prefix bool
		PrefixLength    int
	}{cfg.Resource, duidText, cfg.IAID, cfg.RequestAddress, cfg.RequestPrefix, cfg.PrefixLength})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(domain)
	r := &replication{duid: duidText, domain: hex.EncodeToString(sum[:]), peer: (&url.URL{Scheme: "https", Host: c.PeerAddress.String(), Path: "/v1/pd"}).String()}
	if cfg.Standby {
		r.client = &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: replicaTimeout}
		return r, nil
	}
	r.requests = make(chan chan replicaResult, 1)
	r.done = make(chan struct{})
	r.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/pd" {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if req.Header.Get("X-Routerd-PD-Domain") != r.domain {
			http.Error(w, "not the current PD owner or different PD identity", http.StatusConflict)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), replicaTimeout)
		defer cancel()
		done := make(chan replicaResult, 1)
		select {
		case r.requests <- done:
		case <-ctx.Done():
			return
		}
		select {
		case result := <-done:
			if result.err != nil {
				http.Error(w, result.err.Error(), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("X-Routerd-PD-Domain", r.domain)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(result.data)
		case <-ctx.Done():
		}
	}), ReadHeaderTimeout: time.Second, ReadTimeout: replicaTimeout, WriteTimeout: 2 * replicaTimeout, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.ListenAddress.String())
	if err != nil {
		return nil, err
	}
	go func() { r.err = r.server.Serve(tls.NewListener(listener, serverTLS)); close(r.done) }()
	return r, nil
}
func (r *replication) close() {
	if r.client != nil {
		r.client.CloseIdleConnections()
		return
	}
	r.server.Close()
	<-r.done
}

// Only the DHCP owner reads state and samples its clock. Encoding at this
// boundary avoids sharing the client's mutable assignments with HTTP handlers.
func (r *replication) serve(state checkpoint) error {
	select {
	case <-r.done:
		return fmt.Errorf("PD replication listener: %w", r.err)
	case done := <-r.requests:
		clock, err := boottime.ReadForBoot(state.Snapshot.BootID)
		result := replicaResult{err: err}
		if err == nil {
			result.data, result.err = json.Marshal(replicaReply{State: state, ClockNanos: uint64(clock.BoottimeNanoseconds)})
		}
		done <- result
	default:
	}
	return nil
}

func (r *replication) pull(ctx context.Context, cfg dhcp6config.Config, current checkpoint) (checkpoint, error) {
	// Anchor at request start, before the peer samples its own remaining budget.
	// Transit/processing delay can only shorten the grant; UTC is irrelevant.
	clock, err := boottime.Read()
	if err != nil {
		return checkpoint{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.peer, nil)
	if err != nil {
		return checkpoint{}, err
	}
	req.Header.Set("X-Routerd-PD-Domain", r.domain)
	response, err := r.client.Do(req)
	if err != nil {
		return checkpoint{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Routerd-PD-Domain") != r.domain {
		return checkpoint{}, fmt.Errorf("PD peer unavailable or not owner: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxCheckpointBytes+1))
	if err != nil {
		return checkpoint{}, err
	}
	if len(raw) > maxCheckpointBytes {
		return checkpoint{}, errors.New("PD checkpoint exceeds size bound")
	}
	var reply replicaReply
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return checkpoint{}, err
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return checkpoint{}, errors.New("trailing PD peer snapshot")
	}
	next := reply.State
	if next.Version != stateVersion || next.Mapping != nil || next.Epoch == 0 || next.Revision == 0 || reply.ClockNanos < next.Snapshot.ObservedNanos || reply.ClockNanos > uint64(1<<63-1) {
		return checkpoint{}, errors.New("invalid PD owner checkpoint")
	}
	if err := dhcp6config.ValidateSnapshot(dhcp6config.Config{Resource: cfg.Resource, Interface: next.Snapshot.Interface, IAID: cfg.IAID, RequestAddress: cfg.RequestAddress, RequestPrefix: cfg.RequestPrefix}, next.Snapshot); err != nil {
		return checkpoint{}, err
	}
	if next.Snapshot.ClientDUID != r.duid {
		return checkpoint{}, errors.New("PD peer uses a different DUID")
	}
	if next.Epoch < current.Epoch || next.Epoch == current.Epoch && (next.Source != current.Source || next.Revision < current.Revision) {
		return checkpoint{}, errors.New("stale or conflicting PD owner revision")
	}
	if next.Epoch == current.Epoch && next.Revision == current.Revision {
		if !reflect.DeepEqual(next.Snapshot, current.Snapshot) {
			return checkpoint{}, errors.New("conflicting PD checkpoint")
		}
		return current, nil // Includes the original mapping, never a new deadline.
	}
	next.Mapping = &clockMapping{LocalBootID: clock.BootID, LocalNanos: uint64(clock.BoottimeNanoseconds), SourceNanos: reply.ClockNanos}
	return next, nil
}

func runStandby(ctx context.Context, cfg dhcp6config.Config, r *replication, saved checkpoint, effects Effects, ready func()) error {
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	status := dhcp6config.Snapshot{BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), Resource: cfg.Resource, Interface: cfg.Interface, ClientDUID: cfg.ClientDUID, IAID: cfg.IAID, State: "standby"}
	status.ClientDUID = r.duid
	if ready != nil {
		ready()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		next, pullErr := r.pull(ctx, cfg, saved)
		if ctx.Err() != nil {
			return nil
		}
		sync := &dhcp6config.SyncStatus{Epoch: saved.Epoch, Revision: saved.Revision}
		if status.Sync != nil {
			sync.ReceivedNanos = status.Sync.ReceivedNanos
		}
		if pullErr == nil {
			if next.Epoch != saved.Epoch || next.Revision != saved.Revision {
				if err := writeState(cfg.StatePath, next); err != nil {
					return err
				}
				saved = next
			}
			clock, err = boottime.Read()
			if err != nil {
				return err
			}
			sync.Epoch, sync.Revision, sync.ReceivedNanos = saved.Epoch, saved.Revision, uint64(clock.BoottimeNanoseconds)
		} else {
			sync.Error = pullErr.Error()
		}
		clock, err = boottime.Read()
		if err != nil {
			return err
		}
		status.Sync = sync
		status.ObservedNanos = uint64(clock.BoottimeNanoseconds)
		if err := effects.Update(ctx, status); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.done:
			return r.err
		case <-ticker.C:
		}
	}
}
