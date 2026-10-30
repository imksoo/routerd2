// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) trafficFlows() {
	for _, r := range c.resourcesOfKind("TrafficFlowLog") {
		c.allow(r, "enabled", "path", "source", "includeApplicationLayer", "includeTLSSNI")
		spec := r.Spec.(api.TrafficFlowLogSpec)
		if !spec.Enabled {
			c.target(r, "disabled")
			continue
		}
		if spec.Source != "" && spec.Source != "conntrack" {
			c.reject(r, "source", "unsupported_semantics", "only kernel conntrack observation is supported")
			continue
		}
		if spec.IncludeApplicationLayer || spec.IncludeTLSSNI {
			c.reject(r, "includeApplicationLayer/includeTLSSNI", "unsupported_semantics", "legacy DPI/TLS enrichment requires packet classification; connection counters do not replace it")
			continue
		}
		cfg := trafficflowconfig.Config{Retention: c.flowRetention}
		c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "traffic-flow"), TrafficFlow: &cfg})
		c.note(r, "The existing kernel connection table is sampled every 30 seconds through Go netlink. The old FlowKey/SyncActive transaction and history schema and configured retention are reused without an external conntrack command. Counts require explicitly enabled kernel conntrack accounting for new connections. Short connections between samples can be missed, as in the old observer. Legacy files and earlier rows are not imported; DPI/TLS enrichment is not supplied.")
	}
}
