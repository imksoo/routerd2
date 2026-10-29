// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"sort"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// Source references resolve after all producers have their native identities.
// The condition itself belongs to the resulting resource, not to worker input.
type resourceCondition struct {
	id     string
	source resource
	when   api.ResourceWhenSpec
}

func (c *compiler) when(r resource, w api.ResourceWhenSpec, vrrpNames map[string]string) *resourceprogram.FactCondition {
	var terms []resourceprogram.FactCondition
	// Preserve resourcequery.evalResourceWhen: All takes precedence over Any,
	// and either list takes precedence over State. Empty children mean true.
	if len(w.All) > 0 || len(w.Any) > 0 {
		children := w.All
		all := len(children) > 0
		if !all {
			children = w.Any
		}
		for _, child := range children {
			term := c.when(r, child, vrrpNames)
			if term == nil {
				if !all {
					return nil
				}
				continue
			}
			terms = append(terms, *term)
		}
		if len(terms) == 0 {
			return nil
		}
		if len(terms) == 1 {
			return &terms[0]
		}
		if all {
			return &resourceprogram.FactCondition{All: terms}
		}
		return &resourceprogram.FactCondition{Any: terms}
	}
	keys := make([]string, 0, len(w.State))
	for key := range w.State {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		match := w.State[key]
		if match.Exists != nil || match.Status != "" || match.Contains != "" || match.For != "" || match.MaxAge != "" {
			c.reject(r, "when", "unsupported_semantics", "only supported role/readiness equals/in selectors are imported; status/existence and transition-age timing require their own typed native contract")
			continue
		}
		if match.Equals == "" && len(match.In) == 0 {
			continue
		}
		ref := strings.TrimSpace(key)
		ref = strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
		separator := strings.LastIndexByte(ref, '.')
		if separator < 0 {
			c.reject(r, "when", "unsupported_semantics", "condition must name a resource role, phase or datapathState")
			continue
		}
		base, field := strings.TrimSuffix(ref[:separator], ".status"), ref[separator+1:]
		kind := "VirtualAddress"
		if field == "phase" {
			kind = "DHCPv6PrefixDelegation"
		}
		if declared, _, qualified := strings.Cut(base, "/"); qualified {
			kind = declared
		}
		provider := c.getRef(r, "when", base, kind)
		if provider == nil {
			continue
		}
		values := map[string]resourceprogram.FactCondition{}
		fact := func(name string) resourceprogram.FactCondition { return resourceprogram.FactCondition{Fact: name} }
		applied := fact("network.applied")
		switch {
		case kind == "VirtualAddress" && field == "role":
			name := vrrpNames[provider.Kind+"/"+provider.Metadata.Name]
			if name == "" {
				c.reject(r, "when", "reference", "role condition requires a compiled VRRP election")
				continue
			}
			values["master"], values["backup"] = fact("service."+name+".elected"), fact("service."+name+".backup")
		case kind == "DHCPv6PrefixDelegation" && field == "phase":
			values["Bound"] = fact("service." + c.serviceName(*provider, "pd") + ".prefix_bound")
		case kind == "IPv4StaticAddress" && field == "phase":
			values["Applied"] = applied
		case kind == "IPv6DelegatedAddress" && field == "phase":
			allocation := provider.Spec.(api.IPv6DelegatedAddressSpec)
			pd := c.getRef(*provider, "prefixDelegation", allocation.PrefixDelegation, "DHCPv6PrefixDelegation")
			if pd == nil {
				continue
			}
			values["Applied"] = resourceprogram.FactCondition{All: []resourceprogram.FactCondition{applied, fact("service." + c.serviceName(*pd, "pd") + ".prefix_bound")}}
		case kind == "IPv6RouterAdvertisement" && field == "phase":
			values["Applied"] = fact("service." + c.serviceName(*provider, "ra") + ".sent")
		case kind == "EgressRoutePolicy" && field == "datapathState":
			id := "egress/" + provider.Metadata.Name
			if provider.Spec.(api.EgressRoutePolicySpec).HostTraffic {
				id = "host-route/" + provider.Metadata.Name
			}
			values["Ready"] = fact("resource." + id + ".applied")
		default:
			c.reject(r, "when", "unsupported_semantics", "condition must select a supported role, bound prefix or acknowledged address/RA/egress readiness")
			continue
		}
		condition := func(value string) resourceprogram.FactCondition {
			term, ok := values[value]
			if !ok {
				c.reject(r, "when", "unsupported_semantics", "condition value is unsupported; missing readiness is not its negation")
			}
			return term
		}
		if match.Equals != "" {
			terms = append(terms, condition(match.Equals))
		}
		if len(match.In) > 0 {
			var alternatives []resourceprogram.FactCondition
			for _, value := range match.In {
				alternatives = append(alternatives, condition(value))
			}
			terms = append(terms, resourceprogram.FactCondition{Any: alternatives})
		}
	}
	if len(terms) == 0 {
		return nil
	}
	if len(terms) == 1 {
		return &terms[0]
	}
	return &resourceprogram.FactCondition{All: terms}
}
