// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func (c *compiler) egressPolicy(r resource, elections map[string]string) resourceprogram.Resource {
	c.allow(r, "family", "mode", "sourceCIDRs", "destinationCIDRs", "excludeDestinationCIDRs", "hashFields", "selection", "hysteresis", "candidates")
	s := r.Spec.(api.EgressRoutePolicySpec)
	out := resourceprogram.Resource{ID: "egress/" + r.Metadata.Name, When: c.when(r, s.When, elections)}
	if s.Family != "" && s.Family != "ipv4" || s.HostTraffic || s.Mode != "" && s.Mode != "priority" && s.Mode != "hash" && s.Mode != "mark" || s.Selection != "" && s.Selection != "highest-weight-ready" || len(s.DestinationSetRefs) != 0 || len(s.ExcludeDestinationSetRefs) != 0 {
		c.reject(r, "", "unsupported_semantics", "forwarded IPv4 priority/hash and marked tables require literal selectors and highest-weight-ready selection")
		return out
	}
	policy := resourceprogram.EgressPolicy{Sources: s.SourceCIDRs, Destinations: s.DestinationCIDRs, ExcludeDestinations: s.ExcludeDestinationCIDRs, MarkOnly: s.Mode == "mark", HysteresisMillis: 30000}
	if s.Hysteresis != "" {
		d, err := time.ParseDuration(s.Hysteresis)
		if err != nil || d < 0 || d%time.Millisecond != 0 {
			c.reject(r, "hysteresis", "invalid_value", "hysteresis must be a nonnegative whole-millisecond duration")
		} else {
			policy.HysteresisMillis = int(d / time.Millisecond)
		}
	}
	for _, field := range s.HashFields {
		switch field {
		case "sourceAddress":
		case "destinationAddress":
			policy.HashDestination = true
		default:
			c.reject(r, "hashFields", "unsupported_semantics", "connection routing hashes IPv4 source or source and destination addresses")
		}
	}
	for _, candidate := range s.Candidates {
		if candidate.Enabled != nil && !*candidate.Enabled {
			continue
		}
		group := resourceprogram.EgressGroup{Name: candidate.Name, Weight: candidate.Weight, Priority: candidate.Priority, When: c.when(r, candidate.When, elections)}
		if len(candidate.ReadyWhen) != 0 {
			c.reject(r, "candidates.ready_when", "unsupported_semantics", "candidate readiness must name the typed HealthCheck or underlying tunnel")
		}
		for _, dependency := range candidate.DependsOn {
			ref := c.egressRef(r, "candidates.dependsOn", dependency.Resource, "DSLiteTunnel", "IPv4StaticAddress")
			if ref == nil {
				continue
			}
			if ref.Kind == "DSLiteTunnel" {
				if index, dynamic := c.dynamicTunnels[c.aliases[ref.Kind+"/"+ref.Metadata.Name]]; dynamic {
					group.When = egressAll(group.When, &resourceprogram.FactCondition{Fact: "service." + c.document.Services[index].Name + ".active"})
				}
			} else {
				c.resolveStaticAddress(r, "candidates.dependsOn", api.StatusValueSourceSpec{Resource: dependency.Resource, Field: "address"}, true)
			}
			if ref.Kind == "DSLiteTunnel" && !dependencyPhase(dependency, "Up") || ref.Kind == "IPv4StaticAddress" && !dependencyPhase(dependency, "Applied") {
				c.reject(r, "candidates.dependsOn", "unsupported_semantics", "egress depends on an Up tunnel or Applied static address")
			}
		}
		if candidate.Source != "" {
			c.egressRef(r, "candidates.source", candidate.Source, "Interface", "Link", "DSLiteTunnel")
		}
		iface := first(candidate.Interface, candidate.Device)
		if candidate.DeviceFrom.Resource != "" {
			ref := c.egressRef(r, "candidates.deviceFrom", candidate.DeviceFrom.Resource, "Interface", "Link", "DSLiteTunnel")
			if ref != nil {
				field := "ifname"
				if ref.Kind == "DSLiteTunnel" {
					field = "device"
				}
				if candidate.DeviceFrom.Field != field || candidate.DeviceFrom.Optional {
					c.reject(r, "candidates.deviceFrom", "unsupported_semantics", "deviceFrom must select the declared interface identity")
				}
				resolved := c.aliases[ref.Kind+"/"+ref.Metadata.Name]
				if iface != "" && c.interfaceName(r, "candidates.interface", iface, true) != resolved {
					c.reject(r, "candidates.deviceFrom", "conflict", "deviceFrom and interface must select the same kernel device")
				}
				iface = resolved
			}
		}
		targets := candidate.Targets
		if len(targets) == 0 {
			targets = []api.EgressRoutePolicyTarget{{Name: candidate.Name, Interface: iface, Gateway: candidate.Gateway, GatewayFrom: candidate.GatewayFrom, GatewaySource: candidate.GatewaySource, Table: candidate.EffectiveTable(), Priority: candidate.Priority, Mark: candidate.Mark, Metric: candidate.EffectiveMetric(), HealthCheck: candidate.HealthCheck}}
		} else if candidate.HealthCheck != "" {
			group.When = egressAll(group.When, c.egressHealth(r, candidate.HealthCheck))
		}
		for _, target := range targets {
			name := c.interfaceName(r, "candidates.targets.interface", target.EffectiveInterface(), true)
			if target.GatewayFrom.Resource != "" || target.GatewaySource != "" && target.GatewaySource != "none" && target.GatewaySource != "static" || candidate.PreferredSource != "" {
				c.reject(r, "candidates.gateway", "unsupported_semantics", "this IPv4 policy uses a static gateway or an on-link tunnel")
			}
			if target.Mark <= 0 || uint64(target.Mark) > 1<<32-1 {
				c.reject(r, "candidates.mark", "invalid_value", "routing mark must fit a nonzero uint32")
			}
			native := resourceprogram.EgressTarget{Name: target.Name, Interface: name, Gateway: target.Gateway, Table: target.EffectiveTable(), Priority: target.Priority, Mark: uint32(target.Mark), Metric: target.EffectiveMetric(), When: c.egressHealth(r, target.HealthCheck)}
			if index, dynamic := c.dynamicTunnels[name]; dynamic {
				native.OwnerWhen = &resourceprogram.FactCondition{Fact: "service." + c.document.Services[index].Name + ".active"}
			}
			group.Targets = append(group.Targets, native)
		}
		policy.Groups = append(policy.Groups, group)
	}
	out.EgressPolicy = &policy
	c.target(r, out.ID)
	return out
}

func (c *compiler) egressHealth(r resource, reference string) *resourceprogram.FactCondition {
	if reference == "" {
		return nil
	}
	ref := c.getRef(r, "candidates.healthCheck", reference, "HealthCheck")
	if ref == nil {
		return nil
	}
	return &resourceprogram.FactCondition{Fact: "service." + c.serviceName(*ref, "health") + ".active"}
}

func egressAll(a, b *resourceprogram.FactCondition) *resourceprogram.FactCondition {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &resourceprogram.FactCondition{All: []resourceprogram.FactCondition{*a, *b}}
}

func (c *compiler) egressRef(r resource, field, value string, kinds ...string) *resource {
	kind, _, qualified := strings.Cut(value, "/")
	if qualified {
		for _, allowed := range kinds {
			if kind == allowed {
				return c.getRef(r, field, value, allowed)
			}
		}
	}
	c.reject(r, field, "reference", "egress path reference must name a supported kind and resource")
	return nil
}
