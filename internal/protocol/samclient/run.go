// SPDX-License-Identifier: BSD-3-Clause

// Package samclient owns leaf enrollment, renewal and withdrawal. Existing
// routerd HTTP operations are reused; kernel topology application is separate.
package samclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	oldconfig "github.com/imksoo/routerd/pkg/config"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

type endpoint struct {
	client  *controlapi.Client
	receipt *controlapi.SAMEnrollmentClaimSubmitResult
	expires uint64
	renew   uint64
}

type client struct {
	cfg        samclientconfig.Config
	claim      api.Resource
	digest     string
	endpoints  []endpoint
	operation  sync.Mutex
	mu         sync.Mutex
	snapshot   samclientconfig.Snapshot
	revoked    bool
	revokeDone bool
	publish    func(context.Context, samclientconfig.Snapshot) error
}

// Run owns one configured enrollment and its existing service status lifetime.
// Readiness means its local API is available, not that an RR accepted the claim.
func Run(ctx context.Context, cfg samclientconfig.Config, publish func(context.Context, samclientconfig.Snapshot) error, ready func()) error {
	cfg.Node = samclientconfig.Normalize(cfg.Node)
	if err := samclientconfig.Validate(cfg.Node); err != nil {
		return err
	}
	c := &client{cfg: cfg, digest: samenrollment.ClaimDigest(cfg.Claim), publish: publish}
	c.claim = api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMEnrollmentClaim"}, Metadata: api.ObjectMeta{Name: cfg.ClaimName}, Spec: cfg.Claim}
	c.snapshot = samclientconfig.Snapshot{ClaimRef: "SAMEnrollmentClaim/" + cfg.ClaimName, ClaimDigest: c.digest, Phase: "starting"}
	token := ""
	if cfg.TokenFile != "" {
		data, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return fmt.Errorf("read SAM control token: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return errors.New("SAM control token is empty")
		}
	}
	for _, address := range cfg.Endpoints {
		transport, err := controlapi.NewHTTPClientWithTLS(address, controlapi.TLSOptions{CAFile: cfg.TLS.CAFile, CertFile: cfg.TLS.CertFile, KeyFile: cfg.TLS.KeyFile, ServerName: cfg.TLS.ServerName, InsecureSkipVerify: cfg.TLS.InsecureSkipVerify})
		if err != nil {
			return err
		}
		c.endpoints = append(c.endpoints, endpoint{client: transport.WithBearerToken(token)})
	}
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	c.revoked = cfg.Claim.Revoked || strings.TrimSpace(string(data)) == samenrollment.ClientIdentityDigest(cfg.Claim)
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() {
		delay := time.Duration(0)
		backoff := time.Duration(cfg.RetryMinSeconds) * time.Second
		for {
			timer := time.NewTimer(delay)
			select {
			case <-runCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			c.operation.Lock()
			var operationErr error
			if c.revoked {
				operationErr = c.revoke(runCtx)
			} else {
				operationErr = c.refresh(runCtx)
			}
			withdrawal := c.revoked
			c.operation.Unlock()
			snapshot := c.current()
			if publish != nil && !withdrawal {
				call, stop := context.WithTimeout(runCtx, time.Second)
				err := publish(call, snapshot)
				stop()
				if err != nil && runCtx.Err() == nil {
					slog.Warn("SAM observation delivery failed", "error", err)
				}
			}
			delay = time.Minute
			if operationErr != nil {
				delay = backoff
				backoff = min(backoff*2, time.Duration(cfg.RetryMaxSeconds)*time.Second)
			} else {
				backoff = time.Duration(cfg.RetryMinSeconds) * time.Second
				clock, err := boottime.Read()
				if err == nil && !withdrawal {
					for _, endpoint := range c.endpoints {
						if endpoint.receipt != nil {
							until := time.Duration(cfg.RetryMinSeconds) * time.Second
							if endpoint.renew > uint64(clock.BoottimeNanoseconds) {
								until = time.Duration(endpoint.renew - uint64(clock.BoottimeNanoseconds))
							}
							delay = min(delay, until)
						}
					}
				}
			}
			// Wake before the accepted original deadline even when retries back off.
			clock, err := boottime.Read()
			if err == nil && snapshot.RRSet != nil && snapshot.ExpiresNanos > uint64(clock.BoottimeNanoseconds) {
				delay = min(delay, time.Duration(snapshot.ExpiresNanos-uint64(clock.BoottimeNanoseconds)))
			}
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { localapi.JSON(w, http.StatusOK, c.current()) })
	mux.HandleFunc("POST /v1/revoke", func(w http.ResponseWriter, r *http.Request) {
		c.operation.Lock()
		defer c.operation.Unlock()
		// Persist the operator's withdrawal before contacting RRs. A partial remote
		// failure must not cause this identity to be registered again after restart.
		if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
			localapi.Fail(w, 500, err)
			return
		}
		if err := config.AtomicWrite(cfg.StatePath, []byte(samenrollment.ClientIdentityDigest(cfg.Claim)+"\n")); err != nil {
			localapi.Fail(w, 500, err)
			return
		}
		c.revoked = true
		c.revokeDone = false
		if err := c.revoke(r.Context()); err != nil {
			localapi.Fail(w, http.StatusBadGateway, err)
			return
		}
		localapi.JSON(w, http.StatusOK, c.current())
	})
	return localapi.Serve(runCtx, cfg.SocketPath, mux, ready)
}

func (c *client) current() samclientconfig.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	clock, err := boottime.Read()
	if c.snapshot.RRSet != nil && (err != nil || c.snapshot.BootID != clock.BootID || c.snapshot.ExpiresNanos <= uint64(clock.BoottimeNanoseconds)) {
		c.snapshot.RRSet, c.snapshot.PeerGroup = nil, nil
		c.snapshot.Phase = "expired"
	}
	return c.snapshot
}

func (c *client) revoke(ctx context.Context) error {
	c.mu.Lock()
	c.snapshot.RRSet, c.snapshot.PeerGroup = nil, nil
	c.snapshot.Phase = "revoked"
	c.mu.Unlock()
	var failures []error
	if c.publish != nil {
		call, cancel := context.WithTimeout(ctx, time.Second)
		err := c.publish(call, c.current())
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("publish SAM withdrawal: %w", err))
		}
	}
	if c.revokeDone {
		return errors.Join(failures...)
	}
	for _, endpoint := range c.endpoints {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := endpoint.client.RevokeSAMEnrollmentClaim(call, controlapi.SAMEnrollmentClaimRevokeRequest{Name: c.cfg.ClaimName, Reason: "native client withdrawn"})
		cancel()
		if err == nil && (!result.Revoked || result.ClaimRef != c.snapshot.ClaimRef) {
			err = errors.New("RR did not acknowledge the configured claim withdrawal")
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	err := errors.Join(failures...)
	c.revokeDone = err == nil
	c.mu.Lock()
	c.snapshot.LastError = ""
	if err != nil {
		c.snapshot.LastError = err.Error()
	}
	c.mu.Unlock()
	return err
}

func (c *client) fetch(ctx context.Context, endpoint *endpoint) (*controlapi.SAMEnrollmentTopologyGetResult, error) {
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return endpoint.client.GetSAMEnrollmentTopology(call, controlapi.SAMEnrollmentTopologyGetRequest{Name: strings.TrimPrefix(c.cfg.Claim.RRSetRef, "SAMRRSet/"), ClaimRef: "SAMEnrollmentClaim/" + c.cfg.ClaimName, ClaimDigest: c.digest, ClaimIdentityDigest: samenrollment.ClientIdentityDigest(c.cfg.Claim)})
}

func (c *client) refresh(ctx context.Context) error {
	var selected samclientconfig.Snapshot
	var direct *api.SAMPeerGroupSpec
	directAgreed := c.cfg.Claim.DirectMesh
	var failures []error
	for i := range c.endpoints {
		endpoint := &c.endpoints[i]
		topology, err := c.fetch(ctx, endpoint)
		var remote *controlapi.APIError
		absent := errors.As(err, &remote) && remote.StatusCode == http.StatusBadRequest && remote.Message == controlapi.ErrBadRequest.Error()+": accepted SAMEnrollmentClaim/"+c.cfg.ClaimName+" "+controlapi.SAMEnrollmentTopologyIdentityAbsentMessage
		if err != nil && !absent {
			failures = append(failures, err)
			directAgreed = false
			continue
		}
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		if absent || endpoint.receipt == nil || uint64(clock.BoottimeNanoseconds) >= endpoint.renew {
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			receipt, err := endpoint.client.SubmitSAMEnrollmentClaim(call, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: c.claim})
			cancel()
			if err == nil && (!receipt.Accepted || receipt.ClaimRef != "SAMEnrollmentClaim/"+c.cfg.ClaimName || receipt.DynamicSource != receipt.ClaimRef || !receipt.ExpiresAt.After(receipt.ObservedAt) || !receipt.ExpiresAt.After(clock.WallTime)) {
				err = errors.New("RR returned an invalid admission receipt")
			}
			if err != nil {
				failures = append(failures, err)
				directAgreed = false
				continue
			}
			endpoint.receipt = receipt
			endpoint.expires = uint64(clock.BoottimeNanoseconds) + uint64(receipt.ExpiresAt.Sub(clock.WallTime))
			before := time.Duration(c.cfg.RefreshBeforeSeconds) * time.Second
			lease := receipt.ExpiresAt.Sub(receipt.ObservedAt)
			if before >= lease {
				before = lease / 2
			}
			endpoint.renew = uint64(clock.BoottimeNanoseconds)
			if remaining := endpoint.expires - endpoint.renew; uint64(before) < remaining {
				endpoint.renew = endpoint.expires - uint64(before)
			}
			topology, err = c.fetch(ctx, endpoint)
			if err != nil {
				failures = append(failures, err)
				directAgreed = false
				continue
			}
		}
		rr, group, err := c.decodeTopology(topology)
		if err != nil {
			failures = append(failures, err)
			directAgreed = false
			continue
		}
		if selected.RRSet == nil {
			selected = samclientconfig.Snapshot{ClaimRef: "SAMEnrollmentClaim/" + c.cfg.ClaimName, ClaimDigest: c.digest, Phase: "ready", ObservedAt: endpoint.receipt.ObservedAt, ExpiresAt: endpoint.receipt.ExpiresAt, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: endpoint.expires, RRSet: rr}
			direct = group
		} else {
			if !reflect.DeepEqual(direct, group) {
				directAgreed = false
			}
			if endpoint.expires < selected.ExpiresNanos {
				selected.ExpiresNanos = endpoint.expires
				selected.ExpiresAt = endpoint.receipt.ExpiresAt
			}
		}
		if group == nil {
			directAgreed = false
		}
	}
	err := errors.Join(failures...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if selected.RRSet != nil {
		c.snapshot = selected
	}
	c.snapshot.PeerGroup = nil
	if directAgreed {
		c.snapshot.PeerGroup = direct
	}
	c.snapshot.LastError = ""
	if err != nil {
		c.snapshot.LastError = err.Error()
		c.snapshot.Phase = "unavailable"
		if c.snapshot.RRSet != nil {
			c.snapshot.Phase = "degraded"
		}
	}
	return err
}

func (c *client) decodeTopology(topology *controlapi.SAMEnrollmentTopologyGetResult) (*api.SAMRRSetSpec, *api.SAMPeerGroupSpec, error) {
	if topology.RRSet.Metadata.Name != strings.TrimPrefix(c.cfg.Claim.RRSetRef, "SAMRRSet/") {
		return nil, nil, errors.New("RR returned a different topology")
	}
	if err := oldconfig.ValidateFetchedSAMEnrollmentTopology(topology.RRSet, nil); err != nil {
		return nil, nil, err
	}
	rr, err := topology.RRSet.SAMRRSetSpec()
	if err != nil {
		return nil, nil, err
	}
	if rr.EnrollmentPolicyRef != c.cfg.Claim.PolicyRef {
		return nil, nil, errors.New("RR topology belongs to a different enrollment policy")
	}
	if !c.cfg.Claim.DirectMesh || topology.ClaimDigest != c.digest || topology.PeerGroup == nil {
		return &rr, nil, nil
	}
	// An invalid optional direct group does not discard independently valid RRs.
	if err := oldconfig.ValidateFetchedSAMEnrollmentTopology(topology.RRSet, topology.PeerGroup); err != nil {
		return &rr, nil, nil
	}
	group, err := topology.PeerGroup.SAMPeerGroupSpec()
	if err != nil {
		return &rr, nil, nil
	}
	slices.SortFunc(group.Nodes, func(a, b api.SAMNodeSpec) int { return strings.Compare(a.NodeRef, b.NodeRef) })
	for _, prefixes := range group.OwnedPrefixesByNode {
		slices.Sort(prefixes)
	}
	return &rr, &group, nil
}
