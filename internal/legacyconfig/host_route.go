// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func (c *compiler) hostRoute(r resource, elections map[string]string) resourceprogram.Resource {
	c.allow(r, "family", "hostTraffic", "mode", "candidates")
	s := r.Spec.(api.EgressRoutePolicySpec)
	out := resourceprogram.Resource{ID: "host-route/" + r.Metadata.Name, When: c.when(r, s.When, elections)}
	if s.Family != "ipv6" || !s.HostTraffic || s.Mode != "priority" || len(s.Candidates) != 1 {
		c.reject(r, "", "unsupported_semantics", "IPv6 host routing requires one priority candidate")
		return out
	}
	candidate := s.Candidates[0]
	if candidate.Enabled != nil && !*candidate.Enabled || candidate.GatewayFrom.Resource != "" || candidate.GatewaySource != "static" || candidate.PreferredSource != "interface" || len(candidate.Targets) != 0 || candidate.HealthCheck != "" || len(candidate.DependsOn) != 0 || len(candidate.ReadyWhen) != 0 {
		c.reject(r, "candidates", "unsupported_semantics", "host route selects the preferred installed interface address and a static gateway")
	}
	iface := c.interfaceName(r, "candidates.interface", candidate.EffectiveInterface(), true)
	if candidate.DeviceFrom.Resource != "" || candidate.Source != "" {
		c.reject(r, "candidates", "unsupported_semantics", "host candidate interface must be explicit")
	}
	c.ensureInterface(r, "candidates.interface", iface)
	out.When = egressAll(out.When, c.when(r, candidate.When, elections))
	intent := resourceprogram.HostRoute{Interface: iface, Gateway: candidate.Gateway, Table: candidate.EffectiveTable(), Priority: candidate.Priority, Metric: candidate.EffectiveMetric()}
	priority := 10100
	for _, service := range c.document.Services {
		if service.DSLite != nil && service.DSLite.SourceDelegation != "" {
			intent.Bypasses = append(intent.Bypasses, resourceprogram.HostRouteBypass{Service: service.ModuleName(), Priority: priority})
			priority++
		}
	}
	out.HostRoute = &intent
	c.target(r, out.ID)
	return out
}
