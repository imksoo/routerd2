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
			c.reject(r, "when", "unsupported_semantics", "only VRRP role equals/in selectors are imported; status/existence and transition-age timing require their own typed native contract")
			continue
		}
		if match.Equals == "" && len(match.In) == 0 {
			continue
		}
		ref := strings.TrimSpace(key)
		ref = strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
		base, ok := strings.CutSuffix(ref, ".status.role")
		if !ok {
			base, ok = strings.CutSuffix(ref, ".role")
		}
		if !ok || !strings.HasPrefix(base, "VirtualAddress/") {
			c.reject(r, "when", "unsupported_semantics", "condition must name the role of a declared VRRP VirtualAddress")
			continue
		}
		provider := c.getRef(r, "when", base, "VirtualAddress")
		if provider == nil {
			continue
		}
		name := vrrpNames[provider.Kind+"/"+provider.Metadata.Name]
		if name == "" {
			c.reject(r, "when", "reference", "role condition requires a successfully compiled VRRP election")
			continue
		}
		role := func(value string) resourceprogram.FactCondition {
			suffix := ""
			switch value {
			case "master":
				suffix = ".elected"
			case "backup":
				suffix = ".backup"
			default:
				c.reject(r, "when", "unsupported_semantics", "only observed master and backup roles are imported; unknown is not the negation of master")
			}
			return resourceprogram.FactCondition{Fact: "service." + name + suffix}
		}
		if match.Equals != "" {
			terms = append(terms, role(match.Equals))
		}
		if len(match.In) > 0 {
			var alternatives []resourceprogram.FactCondition
			for _, value := range match.In {
				alternatives = append(alternatives, role(value))
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
