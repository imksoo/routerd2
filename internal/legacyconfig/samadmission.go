// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/mobilityconfig"
	"github.com/imksoo/routerd2/internal/protocol/samadmissionconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) samAdmission() {
	policies := c.resourcesOfKind("SAMEnrollmentPolicy")
	controls := c.resourcesOfKind("ControlAPI")
	if len(policies) == 0 && len(controls) == 0 {
		return
	}
	if len(controls) != 1 {
		for _, r := range policies {
			c.reject(r, "", "explicit_policy_required", "enrollment admission requires one explicitly configured ControlAPI listener")
		}
		for _, r := range controls {
			c.reject(r, "", "unsupported_semantics", "multiple admission listeners need a single selected state owner")
		}
		return
	}
	control := controls[0]
	c.allow(control, "enabled", "listenAddress", "port", "allowCIDRs", "tokenFrom", "tls")
	s := control.Spec.(api.ControlAPISpec)
	if s.Enabled != nil && !*s.Enabled {
		c.target(control, "disabled")
		for _, r := range policies {
			c.reject(r, "", "unsupported_semantics", "the enrollment policy has no enabled admission listener")
		}
		return
	}
	if len(policies) == 0 {
		c.reject(control, "", "unsupported_semantics", "this listener serves explicit SAM enrollment policies; general legacy ControlAPI operations are not implemented")
		return
	}
	if s.TokenFrom.Env != "" || s.TokenFrom.Base64 {
		c.reject(control, "tokenFrom", "unsupported_semantics", "admission uses an explicitly provisioned raw token file")
	}
	if s.TLS.CertFile != "" || s.TLS.KeyFile != "" || s.TLS.ClientCAFile != "" {
		c.reject(control, "tls", "unsupported_semantics", "TLS listener ownership is not implemented by this admission service")
	}
	address := first(s.ListenAddress, "127.0.0.1")
	listen, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		c.reject(control, "listenAddress", "invalid_value", "admission listener must be an IP address")
		return
	}
	port := s.Port
	if port == 0 {
		port = 65432
	}
	if port < 1 || port > 65535 {
		c.reject(control, "port", "invalid_value", "admission port must be 1..65535")
		return
	}
	n := samadmissionconfig.Node{Listen: netip.AddrPortFrom(listen, uint16(port)), TokenFile: s.TokenFrom.File}
	cidrs := s.AllowCIDRs
	if len(cidrs) == 0 {
		cidrs = []string{"127.0.0.0/8", "::1/128"}
	}
	for _, value := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			c.reject(control, "allowCIDRs", "invalid_value", "admission source prefix is invalid")
			continue
		}
		n.AllowCIDRs = append(n.AllowCIDRs, prefix.Masked())
	}
	for _, r := range policies {
		c.allow(r, "transportProfileRef", "rrSetRef", "rrNodeSetRef", "joinTokenFrom", "joinAudience", "allowedLeafIDs", "tunnelAddressPrefixes", "endpointPrefixes", "directMesh", "mobilityPrefixes", "ttl", "wireGuard", "mobilityPoolRefs")
		spec := r.Spec.(api.SAMEnrollmentPolicySpec)
		profile := c.getRef(r, "transportProfileRef", spec.TransportProfileRef, "SAMTransportProfile")
		nodes := c.getRef(r, "rrNodeSetRef", spec.RRNodeSetRef, "SAMNodeSet")
		if profile == nil || nodes == nil {
			continue
		}
		transport := profile.Spec.(api.SAMTransportProfileSpec)
		if transport.Mode != "ipip" || transport.Encryption != "" && transport.Encryption != "none" {
			c.reject(r, "transportProfileRef", "unsupported_semantics", "this admission owner implements the configured unencrypted IPIP transport")
			continue
		}
		compiled := samadmissionconfig.Policy{Name: r.Metadata.Name, Spec: spec, RRSet: api.SAMRRSetSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/" + r.Metadata.Name}, TransportFingerprint: mobilityconfig.SAMTransportMeshFingerprint(transport)}
		for _, node := range nodes.Spec.(api.SAMNodeSetSpec).Nodes {
			if node.RouteReflector {
				compiled.RRSet.Nodes = append(compiled.RRSet.Nodes, node)
			}
		}
		n.Policies = append(n.Policies, compiled)
	}
	if err := samadmissionconfig.Validate(n); err != nil {
		c.reject(control, "", "invalid_value", err.Error())
		return
	}
	name := c.serviceName(control, "sam-rr")
	c.addService(control, serviceconfig.Service{Name: name, SAMAdmission: &n})
	for _, r := range policies {
		c.target(r, "services/"+name+"/sam_admission/policies")
	}
	c.note(control, "This explicit listener serves SAM claim admission, revocation and topology only, retaining source-prefix and bearer-token authentication. General legacy management endpoints are not silently substituted. The worker owns only accepted claims and revoked identities; RR topology and admission policy are resolved here, without copying the complete Router configuration.")
}
