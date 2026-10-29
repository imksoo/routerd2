// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// routerd renders independent client/server fragments into the same chronyd.
// Keep those source-selection rules, with one explicit owner of the clock.
func (c *compiler) ntp(vrrpNames map[string]string) {
	var owner *resource
	node := ntpconfig.Node{}
	for _, r := range c.resources {
		var source string
		var servers, fallback []string
		var from []api.StatusValueSourceSpec
		var when api.ResourceWhenSpec
		switch r.Kind {
		case "NTPClient":
			c.allow(r, "provider", "managed", "source", "interface", "servers", "serverFrom", "fallbackServers", "when")
			s := r.Spec.(api.NTPClientSpec)
			if !s.Managed {
				c.reject(r, "managed", "unsupported_semantics", "unmanaged time-service observation has no native clock ownership")
				continue
			}
			source, servers, fallback, from, when = s.Source, s.Servers, s.FallbackServers, s.ServerFrom, s.When
		case "NTPServer":
			c.allow(r, "provider", "managed", "source", "servers", "serverFrom", "fallbackServers", "listenAddresses", "listenAddressFrom", "allowCIDRs", "allowCIDRFrom", "when")
			s := r.Spec.(api.NTPServerSpec)
			if !s.Managed {
				c.reject(r, "managed", "unsupported_semantics", "unmanaged time-service observation has no native clock ownership")
				continue
			}
			source, servers, fallback, from = s.Source, s.Servers, s.FallbackServers, s.ServerFrom
			addresses := append([]string(nil), s.ListenAddresses...)
			for _, ref := range s.ListenAddressFrom {
				if value := c.dnsStaticAddress(r, "listenAddressFrom", ref); value != "" {
					addresses = append(addresses, value)
				}
			}
			for _, value := range uniqueStrings(addresses) {
				address, err := netip.ParseAddr(value)
				if err != nil {
					c.reject(r, "listenAddresses", "invalid_value", "NTP listener must resolve to an IP address")
					continue
				}
				found := false
				for _, old := range node.Listen {
					if old == address {
						found = true
						break
					}
				}
				if !found {
					node.Listen = append(node.Listen, address)
				}
			}
			for _, value := range s.AllowCIDRs {
				prefix, err := netip.ParsePrefix(value)
				if err != nil {
					c.reject(r, "allowCIDRs", "invalid_value", "NTP access must be explicit IP prefixes")
					continue
				}
				node.Allow = append(node.Allow, prefix.Masked())
			}
			if len(s.AllowCIDRFrom) > 0 {
				c.reject(r, "allowCIDRFrom", "unsupported_semantics", "dynamic NTP access prefixes require an explicit runtime access policy")
			}
		default:
			continue
		}
		if owner == nil {
			selected := r
			owner = &selected
		}
		group := ntpconfig.Source{When: c.when(r, when, vrrpNames)}
		switch source {
		case "", "static":
		case "auto", "dhcp", "dhcpv6":
			if values := compactNTPList(fallback); len(values) > 0 {
				servers = values
			}
			for _, ref := range from {
				switch ref.Field {
				case "sntpServers":
					if index, ok := c.dhcp6Source(r, "serverFrom", ref.Resource); ok {
						group.SourcesFrom = append(group.SourcesFrom, c.document.Services[index].Name)
					}
				case "ntpServers":
					provider := c.getRef(r, "serverFrom", ref.Resource, "DHCPv4Client")
					if provider != nil {
						group.SourcesFrom = append(group.SourcesFrom, c.serviceName(*provider, "dhcp4"))
					}
				default:
					c.reject(r, "serverFrom.field", "unsupported_semantics", "NTP learning must select DHCPv4 ntpServers or DHCPv6 sntpServers")
				}
			}
		default:
			c.reject(r, "source", "invalid_value", "unknown time source selection")
		}
		for _, value := range compactNTPList(servers) {
			group.Servers = append(group.Servers, ntpconfig.Server{Address: value})
		}
		node.Sources = append(node.Sources, group)
	}
	if owner == nil {
		return
	}
	// Hostname sources use the explicitly configured local DNS service. Do not
	// read the development host's resolver or invent public fallback resolvers.
	for _, service := range c.document.Services {
		if service.DNS == nil {
			continue
		}
		for _, listen := range service.DNS.Spec.Listen {
			if listen.Port != 0 && listen.Port != 53 {
				continue
			}
			for _, value := range listen.Addresses {
				address, err := netip.ParseAddr(value)
				if err != nil {
					continue
				}
				if address.IsUnspecified() {
					if address.Is4() {
						address = netip.MustParseAddr("127.0.0.1")
					} else {
						address = netip.IPv6Loopback()
					}
				}
				found := false
				for _, old := range node.DNS {
					if old == address {
						found = true
						break
					}
				}
				if !found {
					node.DNS = append(node.DNS, address)
				}
			}
		}
	}
	name := c.serviceName(*owner, "ntp")
	c.addService(*owner, serviceconfig.Service{Name: name, NTP: &node})
	for _, r := range c.resources {
		if r.Kind == "NTPClient" || r.Kind == "NTPServer" {
			c.target(r, "services/"+name)
			c.note(r, "One bundled foreground chronyd owns clock discipline and LAN service. Client/server source groups retain static/learned/fallback selection independently; client conditions remove only that source group. Hostname resolution uses declared local DNS listeners. No systemd unit, second clock writer or learned value in intent is imported.")
		}
	}
}

// Adapted from routerd pkg/controller/chain/ntp.go. Legacy list elements can
// contain comma/semicolon/whitespace-separated values, including bracketed IPv6.
func compactNTPList(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
		for _, field := range fields {
			server := strings.Trim(field, "[] ")
			if server != "" && !seen[server] {
				seen[server] = true
				out = append(out, server)
			}
		}
	}
	return out
}
