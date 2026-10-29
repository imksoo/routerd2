// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) routerAdvertisements(vrrpNames map[string]string) map[string]*resourceprogram.FactCondition {
	conditions := make(map[string]*resourceprogram.FactCondition)
	for _, r := range c.resourcesOfKind("IPv6RouterAdvertisement") {
		c.allow(r, "interface", "prefixFrom", "rdnss", "rdnssFrom", "dnssl", "dnsslFrom", "mFlag", "oFlag", "mtu", "prfPreference", "preferredLifetime", "validLifetime")
		s := r.Spec.(api.IPv6RouterAdvertisementSpec)
		if s.PrefixFrom.Field != "" && s.PrefixFrom.Field != "address" {
			c.reject(r, "prefixFrom.field", "unsupported_semantics", "delegated advertisements select the address field")
		}
		delegated := c.getRef(r, "prefixFrom.resource", s.PrefixFrom.Resource, "IPv6DelegatedAddress")
		if delegated == nil {
			continue
		}
		ds := delegated.Spec.(api.IPv6DelegatedAddressSpec)
		provider := c.getRef(*delegated, "prefixDelegation", ds.PrefixDelegation, "DHCPv6PrefixDelegation")
		if provider == nil {
			continue
		}
		index, ok := c.pd[provider.Kind+"/"+provider.Metadata.Name]
		if !ok {
			continue
		}
		iface := c.interfaceName(r, "interface", s.Interface, false)
		if c.interfaceName(*delegated, "interface", ds.Interface, false) != iface || ds.PrefixLength == 128 {
			c.reject(r, "prefixFrom", "reference", "RA requires a delegated LAN on its interface")
		}
		c.ensureInterface(r, "interface", iface)
		from := &serviceconfig.RADelegation{Service: c.document.Services[index].Name, Delegation: c.serviceName(*delegated, "lan"), ValidLifetimeSeconds: c.advertisementLifetime(r, "validLifetime", s.ValidLifetime), PreferredLifetimeSeconds: c.advertisementLifetime(r, "preferredLifetime", s.PreferredLifetime)}
		lifetime := 1800
		if from.ValidLifetimeSeconds != nil {
			lifetime = int(*from.ValidLifetimeSeconds)
		}
		cfg := &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: iface, Managed: s.MFlag, OtherConfig: s.OFlag, MTU: s.MTU, RouterPreference: s.PRFPreference, RouterLifetimeSeconds: lifetime, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10, RDNSS: append([]string(nil), s.RDNSS...), DNSSL: append([]string(nil), s.DNSSL...)}, From: from}
		for i, source := range s.RDNSSFrom {
			path := "rdnssFrom[" + strconv.Itoa(i) + "]"
			if source.Resource == "IPv6DelegatedAddress/"+delegated.Metadata.Name && (source.Field == "" || source.Field == "address") {
				from.RDNSSRouter = true
				continue
			}
			if source.Field == "dnsServers" {
				if owner, ok := c.dhcp6Source(r, path+".resource", source.Resource); ok && owner == index {
					from.UseLearnedRDNSS = true
					continue
				}
			}
			if value := c.dnsStaticAddress(r, path, source); value != "" {
				cfg.RDNSS = append(cfg.RDNSS, value)
			}
		}
		for i, source := range s.DNSSLFrom {
			path := "dnsslFrom[" + strconv.Itoa(i) + "]"
			if strings.HasPrefix(source.Resource, "DNSZone/") && source.Field == "zone" {
				if zone := c.getRef(r, path+".resource", source.Resource, "DNSZone"); zone != nil {
					cfg.DNSSL = append(cfg.DNSSL, strings.TrimSuffix(zone.Spec.(api.DNSZoneSpec).Zone, "."))
				}
				continue
			}
			if source.Field == "domainSearch" {
				if owner, ok := c.dhcp6Source(r, path+".resource", source.Resource); ok && owner == index {
					from.UseLearnedDNSSL = true
					continue
				}
			}
			c.reject(r, path, "unsupported_semantics", "DNSSL selects a DNS zone or the delegated prefix provider's domainSearch")
		}
		name := c.serviceName(r, "ra")
		c.addService(r, serviceconfig.Service{Name: name, RA: cfg})
		conditions["service/"+name] = c.appliedDelegationCondition(r, *delegated, s.DependsOn, c.when(r, s.When, vrrpNames))
	}
	return conditions
}

func (c *compiler) appliedDelegationCondition(r resource, delegated resource, dependencies []api.ResourceDependencySpec, when *resourceprogram.FactCondition) *resourceprogram.FactCondition {
	for _, dependency := range dependencies {
		ref := c.getRef(r, "dependsOn", dependency.Resource, "IPv6DelegatedAddress")
		if ref != nil && (ref.ID() != delegated.ID() || !dependencyPhase(dependency, "Applied")) {
			c.reject(r, "dependsOn", "unsupported_semantics", "serving depends on its selected delegated LAN being Applied")
		}
	}
	ds := delegated.Spec.(api.IPv6DelegatedAddressSpec)
	provider := c.getRef(delegated, "prefixDelegation", ds.PrefixDelegation, "DHCPv6PrefixDelegation")
	if provider == nil {
		return when
	}
	available := resourceprogram.FactCondition{Fact: "service." + c.serviceName(*provider, "pd") + ".active"}
	if when == nil {
		return &available
	}
	return &resourceprogram.FactCondition{All: []resourceprogram.FactCondition{*when, available}}
}

func (c *compiler) advertisementLifetime(r resource, field, value string) *uint32 {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	seconds, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		duration, durationErr := time.ParseDuration(value)
		if durationErr != nil || duration < 0 || duration%time.Second != 0 || duration/time.Second > 1<<32-1 {
			c.reject(r, field, "invalid_value", "advertisement lifetime requires nonnegative whole seconds")
			return nil
		}
		seconds = uint64(duration / time.Second)
	}
	result := uint32(seconds)
	return &result
}
