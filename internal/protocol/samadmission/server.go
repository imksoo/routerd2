// SPDX-License-Identifier: BSD-3-Clause
// Package samadmission serves the existing SAM HTTP protocol using resolved
// policy and one typed admission file. It owns no forwarding configuration.
package samadmission

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/samadmissionconfig"
)

type admission struct {
	Claim        api.SAMEnrollmentClaimSpec `json:"claim"`
	ObservedAt   time.Time                  `json:"observed_at"`
	ExpiresAt    time.Time                  `json:"expires_at"`
	BootID       string                     `json:"boot_id"`
	ExpiresNanos uint64                     `json:"expires_nanos"`
}

type savedState struct {
	Claims  map[string]admission `json:"claims"`
	Revoked map[string]time.Time `json:"revoked"`
}

type policy struct {
	samadmissionconfig.Policy
	secret  []byte
	leafIDs *regexp.Regexp
	ttl     time.Duration
}

type server struct {
	cfg      samadmissionconfig.Config
	policies map[string]policy
	token    string
	mu       sync.Mutex
	state    savedState
}

func Run(ctx context.Context, cfg samadmissionconfig.Config, ready func()) error {
	if err := samadmissionconfig.Validate(cfg.Node); err != nil {
		return err
	}
	s := &server{cfg: cfg, policies: map[string]policy{}, state: savedState{Claims: map[string]admission{}, Revoked: map[string]time.Time{}}}
	if cfg.TokenFile != "" {
		data, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return err
		}
		s.token = strings.TrimSpace(string(data))
		if s.token == "" {
			return errors.New("SAM control token is empty")
		}
	}
	for _, value := range cfg.Policies {
		p := policy{Policy: value, ttl: 5 * time.Minute}
		p.leafIDs = regexp.MustCompile(value.Spec.AllowedLeafIDs.Pattern)
		if value.Spec.TTL != "" {
			p.ttl, _ = time.ParseDuration(value.Spec.TTL)
		}
		if value.Spec.JoinTokenFrom.File != "" {
			data, err := os.ReadFile(value.Spec.JoinTokenFrom.File)
			if err != nil {
				return err
			}
			p.secret = []byte(strings.TrimSpace(string(data)))
			if len(p.secret) == 0 {
				return errors.New("SAM join token is empty")
			}
		}
		s.policies["SAMEnrollmentPolicy/"+value.Name] = p
	}
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return fmt.Errorf("read SAM admissions: %w", err)
		}
		if s.state.Claims == nil {
			s.state.Claims = map[string]admission{}
		}
		if s.state.Revoked == nil {
			s.state.Revoked = map[string]time.Time{}
		}
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	// Re-evaluate changed policy once at startup. A restart never grants a new
	// interval: same-boot elapsed expiry or the saved absolute expiry is retained.
	for name, entry := range s.state.Claims {
		if entry.Claim.Revoked {
			continue
		}
		p, ok := s.policies[entry.Claim.PolicyRef]
		if !ok {
			delete(s.state.Claims, name)
			continue
		}
		expires, err := authorize(p, entry.Claim, entry.ObservedAt)
		if err != nil {
			delete(s.state.Claims, name)
			continue
		}
		if expires.Before(entry.ExpiresAt) {
			shortening := uint64(entry.ExpiresAt.Sub(expires))
			if shortening >= entry.ExpiresNanos {
				delete(s.state.Claims, name)
				continue
			}
			entry.ExpiresNanos -= shortening
			entry.ExpiresAt = expires
		}
		if entry.BootID != clock.BootID {
			remaining := entry.ExpiresAt.Sub(clock.WallTime)
			if remaining <= 0 {
				delete(s.state.Claims, name)
				continue
			}
			entry.ExpiresNanos = uint64(clock.BoottimeNanoseconds) + uint64(remaining)
			entry.BootID = clock.BootID
		}
		if !s.active(entry, clock) {
			delete(s.state.Claims, name)
			continue
		}
		s.state.Claims[name] = entry
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen.String())
	if err != nil {
		return err
	}
	defer listener.Close()
	h := controlapi.Handler{SubmitSAMEnrollmentClaim: s.submit, RevokeSAMEnrollmentClaim: s.revoke, GetSAMEnrollmentTopology: s.topology}
	httpServer := &http.Server{Handler: s.admit(h), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			call, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(call)
			_ = httpServer.Close()
		case <-stopped:
		}
	}()
	if ready != nil {
		ready()
	}
	err = httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// The CIDR and constant-time bearer comparison retain the old ControlAPI
// admission behavior; only the three enrollment operations are exposed.
func (s *server) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		address, err := netip.ParseAddr(host)
		allowed := false
		if err == nil {
			for _, prefix := range s.cfg.AllowCIDRs {
				if prefix.Contains(address.Unmap()) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			http.Error(w, "forbidden", 403)
			return
		}
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if s.token != "" && (len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(auth[len(prefix):])), []byte(s.token)) != 1) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="routerd-control-api"`)
			http.Error(w, "unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) save(next savedState) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := config.AtomicWrite(s.cfg.StatePath, data); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *server) active(entry admission, clock boottime.Clock) bool {
	return entry.BootID == clock.BootID && entry.ExpiresNanos > uint64(clock.BoottimeNanoseconds)
}

func (s *server) submit(r *http.Request, req controlapi.SAMEnrollmentClaimSubmitRequest) (*controlapi.SAMEnrollmentClaimSubmitResult, error) {
	if req.Claim.APIVersion != api.MobilityAPIVersion || req.Claim.Kind != "SAMEnrollmentClaim" || strings.TrimSpace(req.Claim.Metadata.Name) == "" {
		return nil, fmt.Errorf("%w: expected a named SAMEnrollmentClaim", controlapi.ErrBadRequest)
	}
	claim, err := req.Claim.SAMEnrollmentClaimSpec()
	if err != nil {
		return nil, fmt.Errorf("%w: invalid claim", controlapi.ErrBadRequest)
	}
	claim.PolicyRef = strings.TrimSpace(claim.PolicyRef)
	claim.RRSetRef = strings.TrimSpace(claim.RRSetRef)
	claim.LeafID = strings.TrimSpace(claim.LeafID)
	claim.JoinAudience = strings.TrimSpace(claim.JoinAudience)
	claim.JoinNonce = strings.TrimSpace(claim.JoinNonce)
	claim.JoinTimestamp = strings.TrimSpace(claim.JoinTimestamp)
	claim.JoinHMAC = strings.TrimSpace(claim.JoinHMAC)
	claim.TunnelAddress = strings.TrimSpace(claim.TunnelAddress)
	claim.Endpoint = strings.TrimSpace(claim.Endpoint)
	claim.ExpiresAt = strings.TrimSpace(claim.ExpiresAt)
	claim.BGP.RouterID = strings.TrimSpace(claim.BGP.RouterID)
	name := strings.TrimSpace(req.Claim.Metadata.Name)
	p, ok := s.policies[strings.TrimSpace(claim.PolicyRef)]
	if !ok {
		return nil, fmt.Errorf("%w: enrollment policy not found", controlapi.ErrBadRequest)
	}
	clock, err := boottime.Read()
	if err != nil {
		return nil, err
	}
	expires, err := authorize(p, claim, clock.WallTime)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", controlapi.ErrBadRequest, err)
	}
	identity := samenrollment.ClientIdentityDigest(claim)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, revoked := s.state.Revoked[identity]; revoked {
		return nil, fmt.Errorf("%w: identity revoked; use a new signed nonce", controlapi.ErrBadRequest)
	}
	if previous, ok := s.state.Claims[name]; ok && previous.Claim.Revoked && strings.TrimSpace(previous.Claim.JoinNonce) != "" && (strings.TrimSpace(claim.JoinNonce) == "" || strings.TrimSpace(claim.JoinNonce) == strings.TrimSpace(previous.Claim.JoinNonce)) {
		return nil, fmt.Errorf("%w: revoked claim requires a new signed nonce", controlapi.ErrBadRequest)
	}
	next := savedState{Claims: maps.Clone(s.state.Claims), Revoked: s.state.Revoked}
	for other, entry := range next.Claims {
		if !s.active(entry, clock) {
			if !entry.Claim.Revoked {
				delete(next.Claims, other)
			}
			continue
		}
		if other == name {
			continue
		}
		if entry.Claim.PolicyRef == claim.PolicyRef && conflicts(entry.Claim, claim) {
			return nil, fmt.Errorf("%w: claim identity or address is already admitted", controlapi.ErrBadRequest)
		}
	}
	entry := admission{Claim: claim, ObservedAt: clock.WallTime, ExpiresAt: expires, BootID: clock.BootID, ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(expires.Sub(clock.WallTime))}
	if previous, ok := next.Claims[name]; ok && samenrollment.ClientIdentityDigest(previous.Claim) == identity && previous.ExpiresNanos < entry.ExpiresNanos {
		entry.ExpiresNanos = previous.ExpiresNanos
		entry.ExpiresAt = previous.ExpiresAt
	}
	next.Claims[name] = entry
	if err := s.save(next); err != nil {
		return nil, err
	}
	source := "SAMEnrollmentClaim/" + name
	result := controlapi.NewSAMEnrollmentClaimSubmitResult(source, source, 1, entry.ObservedAt, entry.ExpiresAt)
	return &result, nil
}

func (s *server) revoke(r *http.Request, req controlapi.SAMEnrollmentClaimRevokeRequest) (*controlapi.SAMEnrollmentClaimRevokeResult, error) {
	name := strings.TrimSpace(req.Name)
	clock, err := boottime.Read()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.state.Claims[name]
	if !ok {
		return nil, fmt.Errorf("%w: active claim not found", controlapi.ErrBadRequest)
	}
	identity := samenrollment.ClientIdentityDigest(entry.Claim)
	next := savedState{Claims: maps.Clone(s.state.Claims), Revoked: maps.Clone(s.state.Revoked)}
	next.Revoked[identity] = clock.WallTime
	entry.Claim.Revoked = true
	entry.ExpiresAt = clock.WallTime
	entry.ExpiresNanos = uint64(clock.BoottimeNanoseconds)
	next.Claims[name] = entry
	if err := s.save(next); err != nil {
		return nil, err
	}
	source := "SAMEnrollmentClaim/" + name
	result := controlapi.NewSAMEnrollmentClaimRevokeResult(source, source, 1, clock.WallTime, clock.WallTime, req.Reason)
	return &result, nil
}

func (s *server) topology(r *http.Request, req controlapi.SAMEnrollmentTopologyGetRequest) (*controlapi.SAMEnrollmentTopologyGetResult, error) {
	clock, err := boottime.Read()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Revoked[req.ClaimIdentityDigest]; ok {
		return nil, fmt.Errorf("%w: accepted %s is revoked", controlapi.ErrBadRequest, req.ClaimRef)
	}
	entry, ok := s.state.Claims[strings.TrimPrefix(req.ClaimRef, "SAMEnrollmentClaim/")]
	if !ok || !s.active(entry, clock) {
		return nil, fmt.Errorf("%w: accepted %s %s", controlapi.ErrBadRequest, req.ClaimRef, controlapi.SAMEnrollmentTopologyIdentityAbsentMessage)
	}
	if req.ClaimIdentityDigest != "" && req.ClaimIdentityDigest != samenrollment.ClientIdentityDigest(entry.Claim) {
		return nil, fmt.Errorf("%w: accepted %s %s", controlapi.ErrBadRequest, req.ClaimRef, controlapi.SAMEnrollmentTopologyIdentityMismatchMessage)
	}
	p, ok := s.policies[entry.Claim.PolicyRef]
	if !ok || p.Spec.RRSetRef != "SAMRRSet/"+req.Name {
		return nil, fmt.Errorf("%w: RR topology is outside this claim's policy", controlapi.ErrBadRequest)
	}
	rr := api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMRRSet"}, Metadata: api.ObjectMeta{Name: req.Name}, Spec: p.RRSet}
	digest := samenrollment.ClaimDigest(entry.Claim)
	var group *api.Resource
	if entry.Claim.DirectMesh && p.Spec.DirectMesh.PeerGroupRef != "" && req.ClaimDigest == digest {
		selection := samenrollment.ActiveClaimSelection{}
		names := make([]string, 0, len(s.state.Claims))
		for name := range s.state.Claims {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			item := s.state.Claims[name]
			if item.Claim.PolicyRef != entry.Claim.PolicyRef || !s.active(item, clock) {
				continue
			}
			tunnel, _ := samenrollment.ParsePrefixOrAddress(item.Claim.TunnelAddress)
			selection.Claims = append(selection.Claims, samenrollment.ActiveClaim{ResourceName: name, Claim: item.Claim, Tunnel: tunnel})
		}
		projected, _ := samenrollment.ActiveDirectMeshTopology(selection, p.Spec, entry.Claim.LeafID, false)
		group = &api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMPeerGroup"}, Metadata: api.ObjectMeta{Name: strings.TrimPrefix(p.Spec.DirectMesh.PeerGroupRef, "SAMPeerGroup/")}, Spec: api.SAMPeerGroupSpec{EnrollmentPolicyRef: entry.Claim.PolicyRef, TransportFingerprint: p.TransportFingerprint, Nodes: projected.Nodes.Nodes, OwnedPrefixesByNode: projected.OwnedPrefixesByNode}}
	}
	result := controlapi.NewSAMEnrollmentTopologyGetResult(req.Name, digest, rr, group)
	return &result, nil
}
