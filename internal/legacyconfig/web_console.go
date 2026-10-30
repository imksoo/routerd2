// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) webConsoles() {
	for _, r := range c.resourcesOfKind("WebConsole") {
		c.allow(r, "enabled", "listenAddress", "listenAddressFrom", "port", "basePath", "title", "links")
		spec := r.Spec.(api.WebConsoleSpec)
		if !api.BoolDefault(spec.Enabled, true) {
			c.target(r, "disabled")
			continue
		}
		if meaningful(child(r.node, "listenAddressFrom")) {
			c.reject(r, "listenAddressFrom", "unsupported_semantics", "console listening requires an explicit local IP; dynamic address ownership is not inferred")
			continue
		}
		address := spec.ListenAddress
		if address == "" {
			address = "127.0.0.1"
		}
		ip, err := netip.ParseAddr(address)
		if err != nil {
			c.reject(r, "listenAddress", "invalid_value", "console listener requires an IP literal")
			continue
		}
		port := spec.Port
		if port == 0 {
			port = 8080
		}
		if port < 1 || port > 65535 {
			c.reject(r, "port", "invalid_value", "console port must be 1..65535")
			continue
		}
		cfg := webconsoleconfig.Config{Listen: netip.AddrPortFrom(ip, uint16(port)), BasePath: spec.BasePath, Title: spec.Title}
		for _, link := range spec.Links {
			cfg.Links = append(cfg.Links, webconsoleconfig.Link{Label: link.Label, URL: link.URL, Description: link.Description})
		}
		cfg = webconsoleconfig.Normalize(cfg)
		if err := webconsoleconfig.Validate(cfg); err != nil {
			c.reject(r, "", "native_validation", err.Error())
			continue
		}
		c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "console"), WebConsole: &cfg})
		c.note(r, "Native read-only HTML console retains listener, base path, title and HTTP(S) links. It reads current typed control health, kernel observation, resource conditions and confirmation metadata; it never exposes a mutation API or raw configuration. History/client/VPN/SAM pages from the old console are not yet implemented; importing this declaration is not complete WebConsole parity.")
	}
}
