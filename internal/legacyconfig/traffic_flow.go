// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
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
		cfg := trafficflowconfig.Config{Retention: c.flowRetention, ApplicationLayer: spec.IncludeApplicationLayer}
		if spec.IncludeTLSSNI || spec.IncludeApplicationLayer {
			group := uint16(2)
			for _, service := range c.document.Services {
				if service.FirewallLog != nil && service.FirewallLog.Group == group {
					group++
				}
			}
			cfg.PacketGroup = group
			if c.document.Network.Firewall == nil {
				c.document.Network.Firewall = &network.Firewall{DisableForward: true}
			}
			c.document.Network.Firewall.FlowObservationGroup = group
			c.target(r, "network.firewall.flow_observation_group")
			c.note(r, "TLS SNI and other built-in packet metadata reuse routerd pkg/dpi directly in the collector. Application classification requires the statically linked libndpi module build. It does not decrypt TLS.")
		}
		c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "traffic-flow"), TrafficFlow: &cfg})
		c.note(r, "The existing kernel connection table is sampled every 30 seconds through Go netlink. The old FlowKey/SyncActive transaction and history schema and configured retention are reused without an external conntrack command. Counts require explicitly enabled kernel conntrack accounting for new connections. Short connections between samples can be missed, as in the old observer. Legacy files and earlier rows are not imported.")
	}
}
