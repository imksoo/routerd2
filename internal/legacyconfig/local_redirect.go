// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
)

// Preserve routerd's local_service_redirect.go match: input interface,
// destination address set, transport and destination port. Kernel redirect
// retains the original destination in conntrack for the reply path.
func (c *compiler) localServiceRedirects() {
	redirects := c.resourcesOfKind("LocalServiceRedirect")
	if len(redirects) == 0 {
		return
	}
	f := c.document.Network.Firewall
	if f == nil {
		f = &network.Firewall{DisableForward: true}
		c.document.Network.Firewall = f
	}
	// The minimal appliance has no host resolver manager. Preserve lookup
	// through the declared local DNS service, never a leftover resolv.conf.
	var resolvers []string
	for _, service := range c.document.Services {
		if service.DNS == nil {
			continue
		}
		for _, listen := range service.DNS.Spec.Listen {
			if listen.Port != 53 {
				continue
			}
			for _, text := range listen.Addresses {
				if a, err := netip.ParseAddr(text); err == nil && a.IsLoopback() {
					resolvers = append(resolvers, net.JoinHostPort(a.String(), strconv.Itoa(listen.Port)))
				}
			}
		}
	}
	for _, r := range c.resourcesOfKind("IPAddressSet") {
		c.allow(r, "addresses", "names", "refreshInterval")
		spec := r.Spec.(api.IPAddressSetSpec)
		set := network.AddressSet{Name: r.Metadata.Name, RefreshInterval: strings.TrimSpace(spec.RefreshInterval)}
		for _, text := range spec.Addresses {
			address, err := netip.ParseAddr(strings.TrimSpace(text))
			if err != nil {
				c.reject(r, "addresses", "invalid_value", "address set entries must be host addresses")
				continue
			}
			set.Addresses = append(set.Addresses, address.Unmap())
		}
		for _, name := range spec.Names {
			if name = strings.TrimSpace(name); name != "" {
				set.Names = append(set.Names, name)
			}
		}
		if len(set.Names) > 0 {
			set.Resolvers = resolvers
		}
		f.AddressSets = append(f.AddressSets, set)
		c.target(r, "network.firewall.address_sets")
	}
	for _, r := range redirects {
		c.allow(r, "interface", "rules")
		spec := r.Spec.(api.LocalServiceRedirectSpec)
		iface := c.filterInterface(r, "interface", spec.Interface)
		for _, rule := range spec.Rules {
			ref := c.byRef[rule.DestinationSetRef]
			if ref == nil || ref.Kind != "IPAddressSet" {
				c.reject(r, "rules.destinationSetRef", "reference", "redirect must reference a declared IPAddressSet")
				continue
			}
			seen := map[string]bool{}
			for _, protocol := range rule.Protocols {
				protocol = strings.ToLower(strings.TrimSpace(protocol))
				if seen[protocol] {
					continue
				}
				seen[protocol] = true
				f.LocalRedirects = append(f.LocalRedirects, network.LocalRedirect{Interface: iface, DestinationSet: ref.Metadata.Name, Protocol: protocol, DestinationPort: rule.DestinationPort, Port: rule.RedirectPort})
			}
		}
		c.target(r, "network.firewall.local_redirects")
	}
}
