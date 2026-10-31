// SPDX-License-Identifier: BSD-3-Clause

// Package samclient owns leaf enrollment, renewal and withdrawal. Existing
// routerd HTTP operations are reused; kernel topology application is separate.
package samclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	oldconfig "github.com/imksoo/routerd/pkg/config"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

type endpoint struct {
	address string
	receipt *controlapi.SAMEnrollmentClaimSubmitResult
	expires uint64
	renew   uint64
}

type client struct {
	cfg                      samclientconfig.Config
	claim                    api.Resource
	digest                   string
	endpoints                []endpoint
	allowIdentityReplacement bool
	operation                sync.Mutex
	mu                       sync.Mutex
	snapshot                 samclientconfig.Snapshot
	revoked                  bool
	revokeDone               bool
	publish                  func(context.Context, samclientconfig.Snapshot) error
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
	c.snapshot = samclientconfig.Snapshot{ClaimRef: c.claimRef(), ClaimDigest: c.digest, Phase: "starting"}
	for _, address := range cfg.Endpoints {
		c.endpoints = append(c.endpoints, endpoint{address: address})
	}
	if err := c.restore(); err != nil {
		return err
	}
	if cfg.Claim.Revoked {
		if err := c.withdraw(); err != nil {
			return fmt.Errorf("persist configured SAM withdrawal: %w", err)
		}
	}
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
			if c.isRevoked() {
				operationErr = c.revoke(runCtx)
			} else {
				operationErr = c.refresh(runCtx)
			}
			withdrawal := c.isRevoked()
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
						if endpoint.receipt == nil {
							continue
						}
						until := time.Duration(cfg.RetryMinSeconds) * time.Second
						if endpoint.renew > uint64(clock.BoottimeNanoseconds) {
							until = time.Duration(endpoint.renew - uint64(clock.BoottimeNanoseconds))
						}
						delay = min(delay, until)
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
		// Durable local withdrawal comes before any remote request. A partial RR
		// failure must never admit this identity again after a worker restart.
		if err := c.withdraw(); err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		if err := c.revoke(r.Context()); err != nil {
			localapi.Fail(w, http.StatusBadGateway, err)
			return
		}
		localapi.JSON(w, http.StatusOK, c.current())
	})
	return localapi.Serve(runCtx, cfg.SocketPath, mux, ready)
}

func (c *client) claimRef() string { return "SAMEnrollmentClaim/" + c.cfg.ClaimName }

func (c *client) isRevoked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revoked
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
	c.snapshot.LastError = ""
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
	transports, err := c.controlClients()
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, transport := range transports {
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := transport.RevokeSAMEnrollmentClaim(call, controlapi.SAMEnrollmentClaimRevokeRequest{Name: c.cfg.ClaimName, Reason: "native client withdrawn"})
			cancel()
			if err == nil && (result == nil || !result.Revoked || result.ClaimRef != c.claimRef()) {
				err = errors.New("RR did not acknowledge the configured claim withdrawal")
			}
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	err = errors.Join(failures...)
	c.revokeDone = err == nil
	c.mu.Lock()
	if err != nil {
		c.snapshot.LastError = err.Error()
	}
	c.mu.Unlock()
	return err
}

func (c *client) controlClients() ([]*controlapi.Client, error) {
	token := ""
	if c.cfg.TokenFile != "" {
		data, err := os.ReadFile(c.cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read SAM control token: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return nil, errors.New("SAM control token is empty")
		}
	}
	transports := make([]*controlapi.Client, 0, len(c.endpoints))
	for _, endpoint := range c.endpoints {
		transport, err := controlapi.NewHTTPClientWithTLS(endpoint.address, controlapi.TLSOptions{CAFile: c.cfg.TLS.CAFile, CertFile: c.cfg.TLS.CertFile, KeyFile: c.cfg.TLS.KeyFile, ServerName: c.cfg.TLS.ServerName, InsecureSkipVerify: c.cfg.TLS.InsecureSkipVerify})
		if err != nil {
			return nil, err
		}
		transports = append(transports, transport.WithBearerToken(token))
	}
	return transports, nil
}

func (c *client) fetch(ctx context.Context, transport *controlapi.Client) (*controlapi.SAMEnrollmentTopologyGetResult, error) {
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return transport.GetSAMEnrollmentTopology(call, controlapi.SAMEnrollmentTopologyGetRequest{Name: strings.TrimPrefix(c.cfg.Claim.RRSetRef, "SAMRRSet/"), ClaimRef: c.claimRef(), ClaimDigest: c.digest, ClaimIdentityDigest: samenrollment.ClientIdentityDigest(c.cfg.Claim)})
}

type identityState uint8

const (
	identityAdmitted identityState = iota
	identityAbsent
	identityMismatch
)

type fetchedTopology struct {
	state identityState
	rr    *api.SAMRRSetSpec
	group *api.SAMPeerGroupSpec
}

// readAll always completes every GET before the caller considers a POST. A
// replica that reports revoke, identity mismatch, malformed topology, or a
// transport error therefore blocks automatic admission at every other RR.
func (c *client) readAll(ctx context.Context, transports []*controlapi.Client) ([]fetchedTopology, error) {
	results := make([]fetchedTopology, len(transports))
	for i, transport := range transports {
		topology, err := c.fetch(ctx, transport)
		if err != nil {
			switch {
			case c.topologyIdentityError(err, controlapi.SAMEnrollmentTopologyIdentityAbsentMessage):
				results[i].state = identityAbsent
				continue
			case c.topologyIdentityError(err, controlapi.SAMEnrollmentTopologyIdentityMismatchMessage):
				results[i].state = identityMismatch
				continue
			default:
				return nil, err
			}
		}
		if topology.ClaimDigest != c.digest {
			results[i].state = identityMismatch
			continue
		}
		rr, group, err := c.decodeTopology(topology)
		if err != nil {
			return nil, err
		}
		results[i] = fetchedTopology{state: identityAdmitted, rr: rr, group: group}
	}
	return results, nil
}

func (c *client) topologyIdentityError(err error, kind string) bool {
	var remote *controlapi.APIError
	return errors.As(err, &remote) && remote.StatusCode == http.StatusBadRequest && remote.Message == controlapi.ErrBadRequest.Error()+": accepted "+c.claimRef()+" "+kind
}

func (c *client) refresh(ctx context.Context) error {
	transports, err := c.controlClients()
	if err != nil {
		return c.degrade(err)
	}
	results, err := c.readAll(ctx, transports)
	if err != nil {
		return c.degrade(err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return c.degrade(err)
	}
	all := func(want identityState) bool {
		for _, result := range results {
			if result.state != want {
				return false
			}
		}
		return true
	}
	onlyAdmittedOrAbsent := true
	absent := false
	for _, result := range results {
		switch result.state {
		case identityAdmitted:
		case identityAbsent:
			absent = true
		default:
			onlyAdmittedOrAbsent = false
		}
	}
	needsAdmission := false
	switch {
	// A restarted RR can lose only its volatile admission record while its
	// peers retain this same identity. All RRs have been queried before this
	// point, so re-admission is safe only for that admitted-or-absent set.
	case onlyAdmittedOrAbsent && absent:
		needsAdmission = true
	case all(identityMismatch) && c.allowIdentityReplacement:
		needsAdmission = true
	case all(identityAdmitted):
		for _, endpoint := range c.endpoints {
			if endpoint.receipt == nil || uint64(clock.BoottimeNanoseconds) >= endpoint.renew {
				needsAdmission = true
				break
			}
		}
	default:
		return c.degrade(errors.New("SAM enrollment identity is not consistently admitted by every RR"))
	}
	if needsAdmission {
		if err := c.admit(ctx, transports); err != nil {
			return c.degrade(err)
		}
		results, err = c.readAll(ctx, transports)
		if err != nil {
			return c.degrade(err)
		}
		if !allAdmitted(results) {
			return c.degrade(errors.New("RR did not attest the submitted SAM identity"))
		}
	}
	snapshot, directErr := c.snapshotFrom(results)
	if err := c.apply(snapshot); err != nil {
		return c.degrade(fmt.Errorf("persist SAM enrollment state: %w", err))
	}
	if directErr != nil {
		return c.degrade(directErr)
	}
	return nil
}

func allAdmitted(results []fetchedTopology) bool {
	for _, result := range results {
		if result.state != identityAdmitted {
			return false
		}
	}
	return true
}

func (c *client) admit(ctx context.Context, transports []*controlapi.Client) error {
	type admission struct {
		receipt *controlapi.SAMEnrollmentClaimSubmitResult
		expires uint64
		renew   uint64
	}
	accepted := make([]admission, len(transports))
	for i, transport := range transports {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		receipt, err := transport.SubmitSAMEnrollmentClaim(call, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: c.claim})
		cancel()
		if err != nil {
			return err
		}
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		if err := c.validateReceipt(receipt, clock); err != nil {
			return err
		}
		expires := uint64(clock.BoottimeNanoseconds) + uint64(receipt.ExpiresAt.Sub(clock.WallTime))
		before := time.Duration(c.cfg.RefreshBeforeSeconds) * time.Second
		lease := receipt.ExpiresAt.Sub(receipt.ObservedAt)
		if before >= lease {
			before = lease / 2
		}
		renew := uint64(clock.BoottimeNanoseconds)
		if remaining := expires - renew; uint64(before) < remaining {
			renew = expires - uint64(before)
		}
		accepted[i] = admission{receipt: receipt, expires: expires, renew: renew}
	}
	for i := range c.endpoints {
		c.endpoints[i].receipt = accepted[i].receipt
		c.endpoints[i].expires = accepted[i].expires
		c.endpoints[i].renew = accepted[i].renew
	}
	return nil
}

func (c *client) validateReceipt(receipt *controlapi.SAMEnrollmentClaimSubmitResult, clock boottime.Clock) error {
	if receipt == nil || !receipt.Accepted || receipt.ClaimRef != c.claimRef() || receipt.DynamicSource != receipt.ClaimRef || !receipt.ExpiresAt.After(receipt.ObservedAt) || !receipt.ExpiresAt.After(clock.WallTime) {
		return errors.New("RR returned an invalid admission receipt")
	}
	return nil
}

func (c *client) snapshotFrom(results []fetchedTopology) (samclientconfig.Snapshot, error) {
	if !allAdmitted(results) {
		return samclientconfig.Snapshot{}, errors.New("SAM topology is not attested by every RR")
	}
	clock, err := boottime.Read()
	if err != nil {
		return samclientconfig.Snapshot{}, err
	}
	if len(c.endpoints) == 0 || c.endpoints[0].receipt == nil || c.endpoints[0].expires <= uint64(clock.BoottimeNanoseconds) {
		return samclientconfig.Snapshot{}, errors.New("SAM topology has no current admission interval")
	}
	selected := samclientconfig.Snapshot{
		ClaimRef:      c.claimRef(),
		ClaimDigest:   c.digest,
		Phase:         "ready",
		ObservedAt:    c.endpoints[0].receipt.ObservedAt,
		ExpiresAt:     c.endpoints[0].receipt.ExpiresAt,
		BootID:        clock.BootID,
		ObservedNanos: uint64(clock.BoottimeNanoseconds),
		ExpiresNanos:  c.endpoints[0].expires,
		RRSet:         results[0].rr,
	}
	for i, result := range results {
		endpoint := c.endpoints[i]
		if endpoint.receipt == nil || endpoint.expires <= uint64(clock.BoottimeNanoseconds) || result.rr == nil {
			return samclientconfig.Snapshot{}, errors.New("SAM topology has no current admission interval")
		}
		if endpoint.expires < selected.ExpiresNanos {
			selected.ObservedAt = endpoint.receipt.ObservedAt
			selected.ExpiresAt = endpoint.receipt.ExpiresAt
			selected.ExpiresNanos = endpoint.expires
		}
	}
	if !c.cfg.Claim.DirectMesh {
		return selected, nil
	}
	var directDigest string
	for i, result := range results {
		if result.group == nil {
			return selected, errors.New("SAM direct topology is not attested by every RR")
		}
		digest, err := directTopologyDigest(result.group)
		if err != nil {
			return selected, err
		}
		if i == 0 {
			directDigest = digest
			selected.PeerGroup = result.group
			continue
		}
		if digest != directDigest {
			selected.PeerGroup = nil
			return selected, errors.New("SAM direct topology differs between RRs")
		}
	}
	return selected, nil
}

func (c *client) apply(snapshot samclientconfig.Snapshot) error {
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
	return c.save()
}

func (c *client) degrade(err error) error {
	c.mu.Lock()
	c.snapshot.PeerGroup = nil
	c.snapshot.LastError = err.Error()
	c.snapshot.Phase = "unavailable"
	if c.snapshot.RRSet != nil {
		c.snapshot.Phase = "degraded"
	}
	c.mu.Unlock()
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
	if !c.cfg.Claim.DirectMesh || topology.PeerGroup == nil {
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
	return &rr, &group, nil
}

// directTopologyDigest follows the old routerd comparison: lists that carry
// set semantics are sorted before hashing, so RR JSON ordering cannot withdraw
// a semantically unchanged direct group.
func directTopologyDigest(group *api.SAMPeerGroupSpec) (string, error) {
	canonical := *group
	canonical.Nodes = append([]api.SAMNodeSpec(nil), group.Nodes...)
	sort.Slice(canonical.Nodes, func(i, j int) bool { return canonical.Nodes[i].NodeRef < canonical.Nodes[j].NodeRef })
	for i := range canonical.Nodes {
		canonical.Nodes[i].MACAddresses = append([]string(nil), canonical.Nodes[i].MACAddresses...)
		sort.Strings(canonical.Nodes[i].MACAddresses)
		canonical.Nodes[i].WireGuard.AllowedIPs = append([]string(nil), canonical.Nodes[i].WireGuard.AllowedIPs...)
		sort.Strings(canonical.Nodes[i].WireGuard.AllowedIPs)
	}
	if group.OwnedPrefixesByNode != nil {
		canonical.OwnedPrefixesByNode = make(map[string][]string, len(group.OwnedPrefixesByNode))
		for node, prefixes := range group.OwnedPrefixesByNode {
			canonical.OwnedPrefixesByNode[node] = append([]string(nil), prefixes...)
			sort.Strings(canonical.OwnedPrefixesByNode[node])
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}
