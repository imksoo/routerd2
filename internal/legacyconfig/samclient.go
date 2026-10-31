// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) samClients() {
	used := make(map[string]bool)
	for _, r := range c.resourcesOfKind("SAMEnrollmentClient") {
		c.allow(r, "claimRef", "bootstrapEndpoints", "controlAPITokenFrom", "controlAPITLS", "rrSocket", "stateTTLRefreshBefore", "retryBackoff")
		s := r.Spec.(api.SAMEnrollmentClientSpec)
		claim := c.getRef(r, "claimRef", s.ClaimRef, "SAMEnrollmentClaim")
		if claim == nil {
			continue
		}
		used[claim.ID()] = true
		c.allow(*claim, "policyRef", "rrSetRef", "leafID", "joinAudience", "joinNonce", "joinTimestamp", "joinHMAC", "tunnelAddress", "endpoint", "wireGuard", "directMesh", "mobility", "bgp", "expiresAt", "revoked")
		if s.RRSocket != "" {
			c.reject(r, "rrSocket", "unsupported_semantics", "native enrollment uses the explicitly configured HTTP(S) bootstrap endpoints")
		}
		if s.ControlAPITokenFrom.Env != "" || s.ControlAPITokenFrom.Base64 {
			c.reject(r, "controlAPITokenFrom", "unsupported_semantics", "native enrollment uses a provisioned raw token file; environment and encoded sources require explicit replacement")
		}
		n := samclientconfig.Node{ClaimName: claim.Metadata.Name, Claim: claim.Spec.(api.SAMEnrollmentClaimSpec), Endpoints: s.BootstrapEndpoints, TokenFile: s.ControlAPITokenFrom.File, TLS: s.ControlAPITLS}
		for _, field := range []struct {
			path, value string
			target      *int
		}{{"stateTTLRefreshBefore", s.StateTTLRefreshBefore, &n.RefreshBeforeSeconds}, {"retryBackoff.min", s.RetryBackoff.Min, &n.RetryMinSeconds}, {"retryBackoff.max", s.RetryBackoff.Max, &n.RetryMaxSeconds}} {
			if field.value == "" {
				continue
			}
			d, err := time.ParseDuration(field.value)
			if err != nil || d < time.Second || d%time.Second != 0 {
				c.reject(r, field.path, "invalid_value", "native enrollment duration must be positive whole seconds")
				continue
			}
			*field.target = int(d / time.Second)
		}
		n = samclientconfig.Normalize(n)
		if err := samclientconfig.Validate(n); err != nil {
			c.reject(r, "", "invalid_value", err.Error())
			continue
		}
		name := c.serviceName(r, "sam")
		c.addService(r, serviceconfig.Service{Name: name, SAMClient: &n})
		c.target(*claim, "services/"+name+"/sam_client/claim")
		c.note(r, "Leaf enrollment submits the configured signed identity, fetches typed RR/direct topology with its original admission deadline, and supports explicit withdrawal. A matching RR IPIP profile is lowered by the native runtime; token files are provisioned separately and not read by import.")
	}
	for _, r := range c.resourcesOfKind("SAMEnrollmentClaim") {
		if !used[r.ID()] {
			c.reject(r, "", "unsupported_semantics", "an enrollment claim requires a configured native SAMEnrollmentClient; server admission is separate")
		}
	}
}
