// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// The active chain emits both a stateless DHCP range and implicit RA. Import
// both through one selected PD allocation, never just the UDP server half.
func (c *compiler) dhcp6Servers(vrrpNames map[string]string) map[string]*resourceprogram.FactCondition {
	conditions := make(map[string]*resourceprogram.FactCondition)
	for _, r := range c.resourcesOfKind("DHCPv6Server") {
		c.allow(r, "server", "managed", "role", "interface", "listenInterfaces", "mode", "delegatedAddress", "defaultRoute", "dnsServers", "dnsServerFrom", "sntpServers", "sntpServerFrom", "domainSearch", "domainSearchFrom", "logDHCP", "leaseTime", "stickyHoldDays")
		s := r.Spec.(api.DHCPv6ServerSpec)
		if s.Server != "" && s.Server != "dnsmasq" || s.Role != "" && s.Role != "server" || s.Mode != "" && s.Mode != "stateless" {
			c.reject(r, "", "unsupported_semantics", "only the local stateless dnsmasq serving path is implemented; stateful, both and transit behavior cannot be dropped")
		}
		policy, ok := c.options.DHCP6Servers[r.Kind+"/"+r.Metadata.Name]
		if !ok {
			c.reject(r, "", "explicit_policy_required", "dhcp6_servers policy must select a delegated_address and router_lifetime_seconds to replace implicit dnsmasq RA and retained option state")
			continue
		}
		c.usePolicy(r, "dhcp6_servers")
		if policy.RouterLifetimeSeconds != nil && (*policy.RouterLifetimeSeconds < 0 || *policy.RouterLifetimeSeconds > 9000 || *policy.RouterLifetimeSeconds > 0 && *policy.RouterLifetimeSeconds < 10) {
			c.policyIssue(r, "dhcp6_servers", "router_lifetime_seconds", "policy_required", "choose zero or a router lifetime of 10..9000 seconds; native PIO lifetimes retain the original PD budgets")
		}
		ifname := c.interfaceName(r, "interface", s.Interface, false)
		c.ensureInterface(ifname)
		for i, ref := range s.ListenInterfaces {
			if c.interfaceName(r, "listenInterfaces["+strconv.Itoa(i)+"]", ref, false) != ifname {
				c.reject(r, "listenInterfaces", "unsupported_semantics", "additional listening interfaces need independent serving intent")
			}
		}
		delegated := c.byRef[policy.DelegatedAddress]
		if delegated == nil || delegated.Kind != "IPv6DelegatedAddress" {
			c.policyIssue(r, "dhcp6_servers", "delegated_address", "policy_reference", "select one existing IPv6DelegatedAddress using its exact Kind/name")
			continue
		}
		if s.DelegatedAddress != "" {
			original := c.getRef(r, "delegatedAddress", s.DelegatedAddress, "IPv6DelegatedAddress")
			if original != nil && original.ID() != delegated.ID() {
				c.policyIssue(r, "dhcp6_servers", "delegated_address", "policy_conflict", "policy must retain the declared delegatedAddress")
			}
		}
		if child(r.node, "defaultRoute") != nil && policy.RouterLifetimeSeconds != nil && s.DefaultRoute != (*policy.RouterLifetimeSeconds > 0) {
			c.policyIssue(r, "dhcp6_servers", "router_lifetime_seconds", "policy_conflict", "router lifetime must retain the declared defaultRoute intent")
		}
		ds := delegated.Spec.(api.IPv6DelegatedAddressSpec)
		provider := c.getRef(*delegated, "prefixDelegation", ds.PrefixDelegation, "DHCPv6PrefixDelegation")
		if provider == nil {
			continue
		}
		index, ok := c.pd[provider.Kind+"/"+provider.Metadata.Name]
		if !ok {
			continue // The provider's diagnostic already prevents publication.
		}
		allocation := c.serviceName(*delegated, "lan")
		cfg := &serviceconfig.DHCP6Server{Interface: ifname, LogDHCP: s.LogDHCP, ServerDUID: policy.ServerDUID, DomainSearch: append([]string(nil), s.DomainSearch...)}
		cfg.DNSServers = c.dhcp6ServerAddresses(r, "dnsServers", s.DNSServers)
		cfg.SNTPServers = c.dhcp6ServerAddresses(r, "sntpServers", s.SNTPServers)
		cfg.SNTPFrom = c.dhcp6ServerSources(r, "sntpServerFrom", "sntpServers", s.SNTPServerFrom)
		var dynamicSearch []api.StatusValueSourceSpec
		for _, source := range s.DomainSearchFrom {
			if strings.HasPrefix(source.Resource, "DNSZone/") && source.Field == "zone" {
				if zone := c.getRef(r, "domainSearchFrom", source.Resource, "DNSZone"); zone != nil {
					cfg.DomainSearch = append(cfg.DomainSearch, strings.TrimSuffix(zone.Spec.(api.DNSZoneSpec).Zone, "."))
				}
			} else {
				dynamicSearch = append(dynamicSearch, source)
			}
		}
		cfg.SearchFrom = c.dhcp6ServerSources(r, "domainSearchFrom", "domainSearch", dynamicSearch)
		for i, source := range s.DNSServerFrom {
			path := "dnsServerFrom[" + strconv.Itoa(i) + "]"
			if source.Field == "address" {
				ref := c.getRef(r, path+".resource", source.Resource, "IPv6DelegatedAddress")
				if ref == nil {
					continue
				}
				if ref.ID() != delegated.ID() {
					c.reject(r, path, "unsupported_semantics", "router DNS must select the serving LAN's delegated allocation")
					continue
				}
				cfg.DNSDelegation = &serviceconfig.DNSDelegation{Service: c.document.Services[index].Name, Delegation: allocation}
				continue
			}
			if source.Field != "dnsServers" {
				c.reject(r, path+".field", "unsupported_semantics", "DNS source must select dnsServers or a delegated LAN address")
				continue
			}
			if provider, ok := c.dhcp6Source(r, path+".resource", source.Resource); ok {
				cfg.DNSFrom = append(cfg.DNSFrom, c.document.Services[provider].Name)
			}
		}
		for _, refs := range [][]string{cfg.DNSFrom, cfg.SearchFrom} {
			for _, ref := range refs {
				if ref != c.document.Services[index].Name {
					c.reject(r, "", "unsupported_semantics", "implicit RA DNS must use the selected PD provider; other RA information sources require explicit native advertisement intent")
				}
			}
		}
		found := false
		for i := range c.document.Services[index].DHCP6.Delegations {
			lan := &c.document.Services[index].DHCP6.Delegations[i]
			if lan.Name != allocation {
				continue
			}
			found = true
			if lan.Interface != ifname {
				c.reject(r, "interface", "reference", "selected delegated address must belong to the serving interface")
			}
			explicit := false
			for _, service := range c.document.Services {
				if service.RA != nil && service.RA.Interface == ifname {
					explicit = true
					if service.RA.From.Service != c.document.Services[index].Name || service.RA.From.Delegation != allocation {
						c.reject(r, "interface", "reference", "DHCPv6 and RA must select the same delegated LAN")
					}
				}
			}
			if !explicit && policy.RouterLifetimeSeconds == nil {
				c.policyIssue(r, "dhcp6_servers", "router_lifetime_seconds", "policy_required", "implicit RA requires an explicit router lifetime")
			}
			if !explicit && policy.RouterLifetimeSeconds != nil {
				name := c.serviceName(r, "ra")
				advertisement := &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: ifname, OtherConfig: true, RouterLifetimeSeconds: *policy.RouterLifetimeSeconds, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10}, From: &serviceconfig.RADelegation{Service: c.document.Services[index].Name, Delegation: allocation, RDNSSRouter: cfg.DNSDelegation != nil, UseLearnedRDNSS: len(cfg.DNSFrom) > 0, UseLearnedDNSSL: len(cfg.SearchFrom) > 0}}
				advertisement.RDNSS = make([]string, len(cfg.DNSServers))
				for j, ip := range cfg.DNSServers {
					advertisement.RDNSS[j] = ip.String()
				}
				advertisement.DNSSL = append([]string(nil), cfg.DomainSearch...)
				c.addService(r, serviceconfig.Service{Name: name, RA: advertisement})
				conditions["service/"+name] = c.appliedDelegationCondition(r, *delegated, s.DependsOn, c.when(r, s.When, vrrpNames))
			}
		}
		if !found {
			c.reject(r, "delegated_address", "reference", "selected delegated allocation was not compiled")
		}

		name := c.serviceName(r, "dhcp6")
		c.addService(r, serviceconfig.Service{Name: name, DHCP6Server: cfg})
		when := c.when(r, s.When, vrrpNames)
		if len(s.DependsOn) > 0 {
			when = c.appliedDelegationCondition(r, *delegated, s.DependsOn, when)
		}
		conditions["service/"+name] = when
		c.note(r, "Explicit replacement: the selected PD allocation owns LAN RA with O set and configured router lifetime, retaining original PD PIO budgets. Stateless DHCPv6 allocates no address leases; leaseTime and stickyHoldDays do not create an IA_NA allocator. The explicit RA service owns its own conditions and packet template. RA RDNSS/DNSSL retain independently selected static/DHCPv6 options and original information deadlines. Static and learned DHCPv6 options are independent; missing/expired information removes only that source's values instead of stopping all serving. No interface-prefix scan, status snapshot or dnsmasq lease file is imported. The policy's explicit server_duid selects the serving identity; when omitted, a persistent native server DUID is created before serving.")
	}
	return conditions
}

func (c *compiler) dhcp6ServerAddresses(r resource, field string, values []string) []netip.Addr {
	var out []netip.Addr
	for i, value := range values {
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			value = value[1 : len(value)-1]
		}
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			c.reject(r, field+"["+strconv.Itoa(i)+"]", "invalid_value", "expected a literal unscoped IPv6 unicast option address; use typed references for learned values")
			continue
		}
		out = append(out, ip)
	}
	return out
}

func (c *compiler) dhcp6ServerSources(r resource, field, option string, sources []api.StatusValueSourceSpec) []string {
	var out []string
	for i, source := range sources {
		path := field + "[" + strconv.Itoa(i) + "]"
		if source.Field != option {
			c.reject(r, path+".field", "unsupported_semantics", "source field must select this option explicitly; arbitrary status expressions are not frozen into intent")
			continue
		}
		index, ok := c.dhcp6Source(r, path+".resource", source.Resource)
		if !ok {
			continue
		}
		out = append(out, c.document.Services[index].Name)
	}
	return out
}
