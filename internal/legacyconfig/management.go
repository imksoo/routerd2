// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
)

// Adapted from routerd pkg/config/management.go. ManagementAccess checks the
// declared path before apply; FirewallZone and the boot SSH config own effects.
func (c *compiler) managementAccess() {
	management := c.resourcesOfKind("ManagementAccess")
	if len(management) == 0 {
		return
	}
	zones := c.resourcesOfKind("FirewallZone")
	trusted := map[string]bool{}
	for _, zone := range zones {
		spec := zone.Spec.(api.FirewallZoneSpec)
		if spec.Role != "mgmt" && spec.Role != "trust" {
			continue
		}
		for _, ref := range spec.Interfaces {
			kind, name, qualified := strings.Cut(ref, "/")
			if !qualified {
				name, kind = ref, "Interface"
			}
			if kind == "Interface" {
				trusted[name] = true
			}
		}
	}
	requireBound := false
	for _, r := range management {
		c.allow(r, "interfaces", "allowSourceCIDRs", "requireWebConsoleBound")
		spec := r.Spec.(api.ManagementAccessSpec)
		if len(spec.Interfaces) == 0 {
			c.reject(r, "interfaces", "required", "management access requires a declared Interface")
		}
		for _, ref := range spec.Interfaces {
			iface := c.getRef(r, "interfaces", ref, "Interface")
			if iface != nil && len(zones) > 0 && !trusted[iface.Metadata.Name] {
				c.reject(r, "interfaces", "management_access", "management interface must belong to a mgmt or trust FirewallZone")
			}
		}
		for _, value := range spec.AllowSourceCIDRs {
			if _, err := netip.ParsePrefix(strings.TrimSpace(value)); err != nil {
				c.reject(r, "allowSourceCIDRs", "invalid_value", "management source entries must be IP prefixes")
			}
		}
		requireBound = requireBound || spec.RequireWebConsoleBound == nil || *spec.RequireWebConsoleBound
		c.target(r, "configuration-validation")
		c.note(r, "Management interface references, management/trust zone membership and required WebConsole binding are checked before import. FirewallZone and explicit boot SSH settings retain ownership of actual access. Legacy allowSourceCIDRs was syntax-checked but never enforced; no source ACL is created from it.")
	}
	for _, r := range c.resourcesOfKind("WebConsole") {
		spec := r.Spec.(api.WebConsoleSpec)
		if !api.BoolDefault(spec.Enabled, true) || !requireBound {
			continue
		}
		listen := strings.TrimSpace(spec.ListenAddress)
		if listen == "0.0.0.0" || listen == "::" {
			c.reject(r, "listenAddress", "management_access", "ManagementAccess requires WebConsole to bind a specific address")
		}
		// WebConsole has no native migration. Its independent unsupported-kind
		// diagnostic remains even when this management check passes.
	}
}
