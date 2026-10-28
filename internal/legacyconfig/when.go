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
			c.reject(r, "when", "unsupported_semantics", "only VRRP role and PD Bound equals/in selectors are imported; status/existence and transition-age timing require their own typed native contract")
			continue
		}
		if match.Equals == "" && len(match.In) == 0 {
			continue
		}
		ref := strings.TrimSpace(key)
		ref = strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
		separator := strings.LastIndexByte(ref, '.')
		if separator < 0 {
			c.reject(r, "when", "unsupported_semantics", "condition must name a VRRP role or PD Bound phase")
			continue
		}
		base, field := strings.TrimSuffix(ref[:separator], ".status"), ref[separator+1:]
		kind := "VirtualAddress"
		if field == "phase" {
			kind = "DHCPv6PrefixDelegation"
		}
		if field != "role" && field != "phase" {
			c.reject(r, "when", "unsupported_semantics", "condition must name a VRRP role or PD Bound phase")
			continue
		}
		provider := c.getRef(r, "when", base, kind)
		if provider == nil {
			continue
		}
		var name string
		var values map[string]string
		if kind == "DHCPv6PrefixDelegation" {
			name = c.serviceName(*provider, "pd")
			// routerd resourcePhase maps Bound/Renewing/Rebinding to Bound.
			// Native active additionally requires an unexpired grant.
			values = map[string]string{"Bound": ".active"}
		} else {
			name = vrrpNames[provider.Kind+"/"+provider.Metadata.Name]
			values = map[string]string{"master": ".elected", "backup": ".backup"}
		}
		if name == "" {
			c.reject(r, "when", "reference", "role condition requires a successfully compiled VRRP election")
			continue
		}
		condition := func(value string) resourceprogram.FactCondition {
			suffix, ok := values[value]
			if !ok {
				c.reject(r, "when", "unsupported_semantics", "only observed master/backup roles and PD Bound are imported; unknown is not their negation")
			}
			return resourceprogram.FactCondition{Fact: "service." + name + suffix}
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
