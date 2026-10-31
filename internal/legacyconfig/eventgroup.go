// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/eventgroupconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) eventGroups() []resourceCondition {
	var conditions []resourceCondition
	for _, peer := range c.resourcesOfKind("EventPeer") {
		spec := peer.Spec.(api.EventPeerSpec)
		ref := "EventGroup/" + strings.TrimPrefix(strings.TrimSpace(spec.GroupRef), "EventGroup/")
		c.getRef(peer, "groupRef", ref, "EventGroup")
	}
	for _, r := range c.resourcesOfKind("EventGroup") {
		c.allow(r, "nodeName", "peersFrom", "retention", "auth", "listen", "replayWindow", "when")
		s := r.Spec.(api.EventGroupSpec)
		cfg := eventgroupconfig.Config{Group: r.Metadata.Name, NodeName: strings.TrimSpace(s.NodeName), SecretFile: strings.TrimSpace(s.Auth.SecretFile), MaxEvents: s.Retention.MaxEvents, ReplayWindow: 5 * time.Minute}
		if s.Auth.Mode != "" && s.Auth.Mode != "hmac" || s.Auth.SecretRef != "" {
			c.reject(r, "auth", "unsupported_semantics", "event federation uses the configured HMAC secret file")
			continue
		}
		if s.Listen.Address != "" {
			address, err := netip.ParseAddr(strings.TrimSpace(s.Listen.Address))
			if err != nil || s.Listen.Port < 1 || s.Listen.Port > 65535 {
				c.reject(r, "listen", "invalid_value", "event receiver requires its explicit IP address and port")
				continue
			}
			cfg.Listen = netip.AddrPortFrom(address, uint16(s.Listen.Port))
		}
		for _, v := range []struct {
			raw, field string
			target     *time.Duration
		}{{s.Retention.MaxAge, "retention.maxAge", &cfg.MaxAge}, {s.ReplayWindow, "replayWindow", &cfg.ReplayWindow}} {
			if v.raw != "" {
				duration, err := time.ParseDuration(v.raw)
				if err != nil || duration < 0 {
					c.reject(r, v.field, "invalid_value", "invalid event federation duration")
				} else {
					*v.target = duration
				}
			}
		}
		index := map[string]int{}
		add := func(p eventgroupconfig.Peer) {
			p.NodeName = strings.TrimSpace(p.NodeName)
			p.Endpoint = strings.TrimSpace(p.Endpoint)
			if p.NodeName == "" || p.NodeName == cfg.NodeName || p.Endpoint == "" {
				return
			}
			if i, ok := index[p.NodeName]; ok {
				cfg.Peers[i] = p
			} else {
				index[p.NodeName] = len(cfg.Peers)
				cfg.Peers = append(cfg.Peers, p)
			}
		}
		for _, source := range s.PeersFrom {
			ref := strings.TrimSpace(source.Resource)
			if source.Optional && c.byRef[ref] == nil {
				continue
			}
			nodes := c.getRef(r, "peersFrom.resource", ref, "SAMNodeSet")
			if nodes == nil {
				continue
			}
			for _, node := range nodes.Spec.(api.SAMNodeSetSpec).Nodes {
				add(eventgroupconfig.Peer{NodeName: node.NodeRef, Endpoint: node.EventEndpoint})
			}
		}
		for _, p := range c.resourcesOfKind("EventPeer") {
			spec := p.Spec.(api.EventPeerSpec)
			if strings.TrimPrefix(strings.TrimSpace(spec.GroupRef), "EventGroup/") != r.Metadata.Name {
				continue
			}
			c.allow(p, "groupRef", "nodeName", "endpoint", "direction", "types", "subjectPrefixes")
			if spec.Direction != "" && spec.Direction != "push" {
				c.reject(p, "direction", "unsupported_semantics", "event federation sends configured push deliveries")
				continue
			}
			add(eventgroupconfig.Peer{NodeName: spec.NodeName, Endpoint: spec.Endpoint, Types: spec.Types, SubjectPrefixes: spec.SubjectPrefixes})
			c.target(p, "services/"+c.serviceName(r, "event-group")+"/event_group/peers")
		}
		if err := eventgroupconfig.Validate(cfg); err != nil {
			c.reject(r, "", "invalid_value", err.Error())
			continue
		}
		name := c.serviceName(r, "event-group")
		c.addService(r, serviceconfig.Service{Name: name, EventGroup: &cfg})
		conditions = append(conditions, resourceCondition{id: "service/" + name, source: r, when: s.When})
		c.note(r, "Static peer endpoints are resolved from SAMNodeSet; explicit EventPeer entries override by node identity. HMAC federation keeps original event expiry and only forwards local-origin facts. Storage and delivery cursors remain in the native event journal; this service does not implement MobilityPool ownership or network actions.")
	}
	return conditions
}
