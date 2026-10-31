// SPDX-License-Identifier: BSD-3-Clause
package samadmission

import (
	"crypto/hmac"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/samenrollment"
)

// The claim shape, policy ranges, nonce/audience and HMAC checks retain the
// effective routerd admission rules without rebuilding its full Router graph.
func authorize(p policy, c api.SAMEnrollmentClaimSpec, now time.Time) (time.Time, error) {
	if c.Revoked {
		return time.Time{}, errors.New("a revoked claim cannot be submitted")
	}
	if strings.TrimSpace(c.LeafID) == "" || !p.leafIDs.MatchString(strings.TrimSpace(c.LeafID)) {
		return time.Time{}, errors.New("leaf identity is outside policy")
	}
	if strings.TrimSpace(c.RRSetRef) != p.Spec.RRSetRef {
		return time.Time{}, errors.New("claim RR set does not match its policy")
	}
	if c.DirectMesh && p.Spec.DirectMesh.PeerGroupRef == "" {
		return time.Time{}, errors.New("policy does not authorize direct peers")
	}
	if c.WireGuard.PublicKey != "" || c.WireGuard.Endpoint != "" || len(c.WireGuard.AllowedIPs) > 0 || c.WireGuard.PersistentKeepalive != 0 {
		return time.Time{}, errors.New("this admission owner serves the configured unencrypted IPIP transport")
	}
	tunnel, err := samenrollment.ParsePrefixOrAddress(c.TunnelAddress)
	if err != nil || !tunnel.Addr().Is4() || tunnel.Bits() != 32 || !samenrollment.PrefixContains(p.Spec.TunnelAddressPrefixes, tunnel.Addr()) {
		return time.Time{}, errors.New("tunnel address is outside policy")
	}
	if c.Endpoint != "" {
		address, err := netip.ParseAddr(strings.TrimSpace(c.Endpoint))
		if err != nil || !address.Is4() || len(p.Spec.EndpointPrefixes) > 0 && !samenrollment.PrefixContains(p.Spec.EndpointPrefixes, address) {
			return time.Time{}, errors.New("IPIP endpoint is outside policy")
		}
	}
	if c.BGP.RouterID != "" {
		address, err := netip.ParseAddr(strings.TrimSpace(c.BGP.RouterID))
		if err != nil || !address.Is4() {
			return time.Time{}, errors.New("BGP router identity must be IPv4")
		}
	}
	seen := map[netip.Addr]bool{}
	for _, value := range c.Mobility.OwnedAddresses {
		address, err := samenrollment.ParsePrefixOrAddress(value)
		if err != nil || !address.Addr().Is4() || address.Bits() != 32 || !samenrollment.PrefixContains(p.Spec.MobilityPrefixes, address.Addr()) {
			return time.Time{}, errors.New("mobility address is outside policy")
		}
		if seen[address.Addr()] {
			return time.Time{}, errors.New("mobility address is repeated")
		}
		seen[address.Addr()] = true
	}
	if len(p.secret) > 0 {
		if strings.TrimSpace(c.JoinNonce) == "" || strings.TrimSpace(c.JoinTimestamp) == "" {
			return time.Time{}, errors.New("signed enrollment requires its nonce and timestamp")
		}
		if p.Spec.JoinAudience != "" && strings.TrimSpace(c.JoinAudience) != p.Spec.JoinAudience {
			return time.Time{}, errors.New("join audience does not match policy")
		}
		if !hmac.Equal([]byte(samenrollment.JoinHMAC(p.secret, c)), []byte(strings.TrimSpace(c.JoinHMAC))) {
			return time.Time{}, errors.New("join signature is invalid")
		}
	}
	expires := now.Add(p.ttl)
	if c.JoinTimestamp != "" {
		joined, err := samenrollment.ParseTime(c.JoinTimestamp)
		if err != nil {
			return time.Time{}, errors.New("invalid join timestamp")
		}
		// ActiveClaims also limits authority to the signed timestamp plus policy TTL.
		// A repeated submission must not make its receipt outlive that authority.
		if limit := joined.Add(p.ttl); limit.Before(expires) {
			expires = limit
		}
	}
	if c.ExpiresAt != "" {
		requested, err := samenrollment.ParseTime(c.ExpiresAt)
		if err != nil {
			return time.Time{}, errors.New("invalid claim expiry")
		}
		if requested.After(expires) {
			return time.Time{}, errors.New("claim expiry exceeds policy TTL")
		}
		expires = requested
	}
	if !expires.After(now) {
		return time.Time{}, errors.New("claim authority expired")
	}
	return expires, nil
}

func conflicts(a, b api.SAMEnrollmentClaimSpec) bool {
	if strings.TrimSpace(a.LeafID) == strings.TrimSpace(b.LeafID) || a.JoinNonce != "" && strings.TrimSpace(a.JoinNonce) == strings.TrimSpace(b.JoinNonce) {
		return true
	}
	x, _ := samenrollment.ParsePrefixOrAddress(a.TunnelAddress)
	y, _ := samenrollment.ParsePrefixOrAddress(b.TunnelAddress)
	if x == y {
		return true
	}
	if a.BGP.RouterID != "" && strings.TrimSpace(a.BGP.RouterID) == strings.TrimSpace(b.BGP.RouterID) {
		return true
	}
	owned := make(map[netip.Prefix]bool, len(a.Mobility.OwnedAddresses))
	for _, value := range a.Mobility.OwnedAddresses {
		address, _ := samenrollment.ParsePrefixOrAddress(value)
		owned[address] = true
	}
	for _, value := range b.Mobility.OwnedAddresses {
		address, _ := samenrollment.ParsePrefixOrAddress(value)
		if owned[address] {
			return true
		}
	}
	return false
}
