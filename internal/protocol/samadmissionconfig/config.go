// SPDX-License-Identifier: BSD-3-Clause
// Package samadmissionconfig contains resolved admission intent, not a Router.
package samadmissionconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

type Policy struct {
	Name                 string                      `json:"name"`
	Spec                 api.SAMEnrollmentPolicySpec `json:"spec"`
	RRSet                api.SAMRRSetSpec            `json:"rr_set"`
	TransportFingerprint string                      `json:"transport_fingerprint,omitempty"`
}

type Node struct {
	Listen     netip.AddrPort `json:"listen"`
	AllowCIDRs []netip.Prefix `json:"allow_cidrs"`
	TokenFile  string         `json:"token_file,omitempty"`
	Policies   []Policy       `json:"policies"`
}

type Config struct {
	Node
	StatePath string `json:"state_path"`
}

func Validate(c Node) error {
	if !c.Listen.IsValid() || c.Listen.Port() == 0 {
		return errors.New("SAM admission requires an IP listener and port")
	}
	if len(c.AllowCIDRs) == 0 {
		return errors.New("SAM admission requires its configured source prefixes")
	}
	for _, prefix := range c.AllowCIDRs {
		if !prefix.IsValid() {
			return errors.New("invalid SAM control source prefix")
		}
	}
	if c.TokenFile != "" && !filepath.IsAbs(c.TokenFile) {
		return errors.New("SAM control token requires an absolute file path")
	}
	if len(c.Policies) == 0 {
		return errors.New("SAM admission requires an enrollment policy")
	}
	names, rrNames, groups := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range c.Policies {
		if p.Name == "" || names[p.Name] {
			return errors.New("SAM policy identity is empty or repeated")
		}
		names[p.Name] = true
		s := p.Spec
		if !strings.HasPrefix(s.RRSetRef, "SAMRRSet/") || strings.TrimPrefix(s.RRSetRef, "SAMRRSet/") == "" || rrNames[s.RRSetRef] {
			return errors.New("SAM policies require distinct RR set names")
		}
		rrNames[s.RRSetRef] = true
		if p.RRSet.EnrollmentPolicyRef != "SAMEnrollmentPolicy/"+p.Name || len(p.RRSet.Nodes) == 0 {
			return errors.New("SAM policy requires its resolved RR nodes")
		}
		for _, node := range p.RRSet.Nodes {
			if !node.RouteReflector || node.NodeRef == "" || node.SAMEndpointFrom.Resource != "" {
				return errors.New("SAM RR nodes must have static reflector identity")
			}
		}
		if s.JoinTokenFrom.Env != "" || s.JoinTokenFrom.Base64 {
			return errors.New("SAM join authentication requires a provisioned raw token file")
		}
		if s.JoinTokenFrom.File != "" && !filepath.IsAbs(s.JoinTokenFrom.File) {
			return errors.New("SAM join token requires an absolute path")
		}
		if s.WireGuard.Interface != "" || len(s.MobilityPoolRefs) != 0 {
			return errors.New("encrypted admission and referenced mobility pools need explicit native owners")
		}
		if s.TTL != "" {
			duration, err := time.ParseDuration(s.TTL)
			if err != nil || duration <= 0 {
				return errors.New("SAM policy TTL must be positive")
			}
		}
		if _, err := regexp.Compile(s.AllowedLeafIDs.Pattern); err != nil {
			return fmt.Errorf("SAM leaf pattern: %w", err)
		}
		if len(s.TunnelAddressPrefixes) == 0 {
			return errors.New("SAM policy requires its tunnel address prefixes")
		}
		for _, prefixes := range [][]string{s.TunnelAddressPrefixes, s.EndpointPrefixes, s.MobilityPrefixes} {
			for _, value := range prefixes {
				p, err := netip.ParsePrefix(value)
				if err != nil || !p.Addr().Is4() {
					return errors.New("IPIP admission prefixes must be IPv4")
				}
			}
		}
		if group := s.DirectMesh.PeerGroupRef; group != "" {
			if !strings.HasPrefix(group, "SAMPeerGroup/") || strings.TrimPrefix(group, "SAMPeerGroup/") == "" || groups[group] || p.TransportFingerprint == "" {
				return errors.New("SAM direct groups require distinct names and resolved transport identity")
			}
			groups[group] = true
		}
	}
	return nil
}
