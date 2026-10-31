// SPDX-License-Identifier: BSD-3-Clause

// Package samclientconfig holds enrollment intent and typed observations without
// linking the HTTP enrollment engine into the supervisor.
package samclientconfig

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
)

type Node struct {
	ClaimName            string                      `json:"claim_name"`
	Claim                api.SAMEnrollmentClaimSpec  `json:"claim"`
	Endpoints            []string                    `json:"endpoints"`
	TokenFile            string                      `json:"token_file,omitempty"`
	TLS                  api.ControlAPIClientTLSSpec `json:"tls,omitempty"`
	RefreshBeforeSeconds int                         `json:"refresh_before_seconds,omitempty"`
	RetryMinSeconds      int                         `json:"retry_min_seconds,omitempty"`
	RetryMaxSeconds      int                         `json:"retry_max_seconds,omitempty"`
}

type Config struct {
	Node
	SocketPath string `json:"socket_path"`
	StatePath  string `json:"state_path"`
}

// Snapshot carries the server's original admission interval. ExpiresNanos is
// derived once on receipt, so wall-clock changes cannot prolong its authority.
// RRSet remains independently useful when the optional direct group disagrees.
type Snapshot struct {
	ClaimRef      string                `json:"claim_ref"`
	ClaimDigest   string                `json:"claim_digest"`
	Phase         string                `json:"phase"`
	ObservedAt    time.Time             `json:"observed_at,omitempty"`
	ExpiresAt     time.Time             `json:"expires_at,omitempty"`
	BootID        string                `json:"boot_id,omitempty"`
	ObservedNanos uint64                `json:"observed_nanos,omitempty"`
	ExpiresNanos  uint64                `json:"expires_nanos,omitempty"`
	RRSet         *api.SAMRRSetSpec     `json:"rr_set,omitempty"`
	PeerGroup     *api.SAMPeerGroupSpec `json:"peer_group,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
}

func Normalize(c Node) Node {
	c.ClaimName = strings.TrimSpace(c.ClaimName)
	c.Endpoints = append([]string(nil), c.Endpoints...)
	for i := range c.Endpoints {
		c.Endpoints[i] = strings.TrimRight(strings.TrimSpace(c.Endpoints[i]), "/")
	}
	if c.RefreshBeforeSeconds == 0 {
		c.RefreshBeforeSeconds = 600
	}
	if c.RetryMinSeconds == 0 {
		c.RetryMinSeconds = 10
	}
	if c.RetryMaxSeconds == 0 {
		c.RetryMaxSeconds = 900
	}
	return c
}

func Validate(c Node) error {
	c = Normalize(c)
	if c.ClaimName == "" || strings.ContainsAny(c.ClaimName, "/\x00\r\n") {
		return errors.New("SAM claim name is required")
	}
	if !strings.HasPrefix(c.Claim.PolicyRef, "SAMEnrollmentPolicy/") || strings.TrimPrefix(c.Claim.PolicyRef, "SAMEnrollmentPolicy/") == "" {
		return errors.New("SAM claim must name its enrollment policy")
	}
	if !strings.HasPrefix(c.Claim.RRSetRef, "SAMRRSet/") || strings.TrimPrefix(c.Claim.RRSetRef, "SAMRRSet/") == "" {
		return errors.New("SAM claim must name its RR set")
	}
	if c.Claim.LeafID == "" || c.Claim.TunnelAddress == "" {
		return errors.New("SAM claim requires leaf identity and tunnel address")
	}
	if len(c.Endpoints) == 0 {
		return errors.New("SAM enrollment requires a bootstrap endpoint")
	}
	for _, endpoint := range c.Endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("SAM bootstrap endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if c.TokenFile != "" && !filepath.IsAbs(c.TokenFile) {
		return errors.New("SAM control token requires an absolute file path")
	}
	if c.RefreshBeforeSeconds < 1 || c.RetryMinSeconds < 1 || c.RetryMaxSeconds < c.RetryMinSeconds {
		return errors.New("SAM refresh and retry durations must be positive; retry maximum must cover its minimum")
	}
	return nil
}

func ValidateSnapshot(s Snapshot) error {
	if !strings.HasPrefix(s.ClaimRef, "SAMEnrollmentClaim/") || s.ClaimDigest == "" {
		return errors.New("SAM observation requires claim identity")
	}
	switch s.Phase {
	case "starting", "unavailable", "revoked", "expired", "ready", "degraded":
	default:
		return fmt.Errorf("unknown SAM enrollment phase %q", s.Phase)
	}
	if s.RRSet != nil {
		if !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ExpiresNanos <= s.ObservedNanos || !s.ExpiresAt.After(s.ObservedAt) {
			return errors.New("SAM topology requires the original finite admission interval")
		}
	}
	if s.PeerGroup != nil && s.RRSet == nil {
		return errors.New("direct SAM peers require their admitted RR topology")
	}
	return nil
}
