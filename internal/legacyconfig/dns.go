// SPDX-License-Identifier: BSD-3-Clause
//
// DNS resource composition and URL defaults are adapted from
// routerd/pkg/controller/dnsresolver at SourceRevision; see NOTICE.

package legacyconfig

import (
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) dns(vrrpNames map[string]string) {
	var resolvers, forwarders, zones []resource
	upstreams := map[string]resource{}
	usedForwarders, usedUpstreams := map[string]bool{}, map[string]bool{}
	for _, r := range c.resources {
		switch r.Kind {
		case "DNSResolver":
			c.allow(r, "listen", "cache", "metrics", "queryLog")
			resolvers = append(resolvers, r)
		case "DNSForwarder":
			c.allow(r, "resolver", "match", "zoneRefs", "upstreams", "dnssecValidate", "queryTimeout", "healthcheck")
			forwarders = append(forwarders, r)
		case "DNSUpstream":
			c.allow(r, "protocol", "address", "addressFrom", "port", "path", "tlsName", "bootstrap", "bootstrapFrom", "sourceInterface")
			upstreams[r.Metadata.Name] = r
		case "DNSZone":
			c.allow(r, "zone", "ttl", "dnssec", "records", "dhcpDerived", "reverseZones", "ownerRefs")
			zones = append(zones, r)
		}
	}
	for _, r := range resolvers {
		var owners []serviceconfig.DNSUpstreamOwner
		spec := r.Spec.(api.DNSResolverSpec)
		spec.Listen = append([]api.DNSResolverListenSpec(nil), spec.Listen...)
		if spec.QueryLog.Path != "" {
			c.note(r, "Query logging is preserved in the resolver-owned native state directory; the legacy file path and existing history are not imported.")
			spec.QueryLog.Path = ""
		}
		for i := range spec.Listen {
			listen := &spec.Listen[i]
			if len(listen.AddressSources) != 0 {
				c.reject(r, "listen["+strconv.Itoa(i)+"].addressSources", "unsupported_semantics", "deprecated address expressions must be replaced by explicit static addresses")
			}
			listen.Addresses = append([]string(nil), listen.Addresses...)
			var dynamic []api.StatusValueSourceSpec
			for j, source := range listen.AddressFrom {
				path := "listen[" + strconv.Itoa(i) + "].addressFrom[" + strconv.Itoa(j) + "]"
				ref := c.byRef[source.Resource]
				if ref != nil && ref.Kind == "Interface" {
					dynamic = append(dynamic, api.StatusValueSourceSpec{Resource: "Interface/" + c.interfaceName(r, path, source.Resource, false), Field: source.Field, Optional: source.Optional})
				} else if ref != nil && ref.Kind == "VirtualAddress" && ref.Spec.(api.VirtualAddressSpec).Mode == "vrrp" {
					dynamic = append(dynamic, api.StatusValueSourceSpec{Resource: "VirtualAddress/" + c.serviceName(*ref, "vrrp"), Field: source.Field, Optional: source.Optional})
				} else if ref != nil && ref.Kind == "IPv6DelegatedAddress" {
					ds := ref.Spec.(api.IPv6DelegatedAddressSpec)
					provider := c.getRef(*ref, "prefixDelegation", ds.PrefixDelegation, "DHCPv6PrefixDelegation")
					if provider == nil {
						continue
					}
					if index, ok := c.pd[provider.Kind+"/"+provider.Metadata.Name]; ok {
						dynamic = append(dynamic, api.StatusValueSourceSpec{Resource: "IPv6DelegatedAddress/" + c.document.Services[index].Name + "/" + c.serviceName(*ref, "lan"), Field: source.Field, Optional: source.Optional})
					} else {
						c.reject(r, path, "reference", "delegated DNS listener requires a compiled PD provider")
					}
				} else if address := c.dnsStaticAddress(r, path, source); address != "" {
					listen.Addresses = append(listen.Addresses, address)
				}
			}
			listen.Addresses = uniqueStrings(listen.Addresses)
			listen.AddressFrom = dynamic
			listen.AddressSources = nil
		}
		for _, forward := range forwarders {
			fs := forward.Spec.(api.DNSForwarderSpec)
			resolver := c.getRef(forward, "resolver", fs.Resolver, "DNSResolver")
			if resolver == nil || resolver.Metadata.Name != r.Metadata.Name {
				continue
			}
			usedForwarders[forward.ID()] = true
			source := api.DNSResolverSourceSpec{
				Name: forward.Metadata.Name, Match: append([]string(nil), fs.Match...),
				DNSSECValidate: fs.DNSSECValidate, QueryTimeout: fs.QueryTimeout,
				Healthcheck: fs.Healthcheck,
			}
			if len(fs.ZoneRefs) > 0 {
				source.Kind = "zone"
				if len(fs.Upstreams) > 0 {
					c.reject(forward, "upstreams", "invalid_value", "one forwarder cannot select both local zones and upstreams")
				}
				for _, ref := range fs.ZoneRefs {
					if zone := c.getRef(forward, "zoneRefs", ref, "DNSZone"); zone != nil {
						source.ZoneRef = append(source.ZoneRef, zone.Metadata.Name)
					}
				}
			} else {
				source.Kind = "forward"
				for _, match := range fs.Match {
					if strings.TrimSpace(match) == "." {
						source.Kind = "upstream"
					}
				}
				for _, ref := range fs.Upstreams {
					upstream := c.getRef(forward, "upstreams", ref, "DNSUpstream")
					if upstream == nil {
						continue
					}
					usedUpstreams[upstream.Metadata.Name] = true
					us := upstream.Spec.(api.DNSUpstreamSpec)
					if strings.TrimSpace(us.Address) != "" {
						if raw := c.dnsUpstreamURL(*upstream, us); raw != "" {
							source.Upstreams = append(source.Upstreams, raw)
						}
					} else if len(us.AddressFrom) == 0 {
						c.reject(*upstream, "address", "required", "DNS upstream requires an address or learned DNS source")
					}
					learned := c.dnsLearnedSources(*upstream, "addressFrom", us.AddressFrom)
					source.UpstreamFrom = append(source.UpstreamFrom, learned...)
					if when := c.when(*upstream, us.When, vrrpNames); when != nil {
						owner := ""
						for _, name := range vrrpNames {
							if reflect.DeepEqual(when, &resourceprogram.FactCondition{Fact: "service." + name + ".elected"}) {
								owner = name
								break
							}
						}
						if owner == "" || us.Address != "" || len(us.Bootstrap) != 0 || len(us.BootstrapFrom) != 0 || len(learned) == 0 {
							c.reject(*upstream, "when", "unsupported_semantics", "conditional DNS upstream requires learned DNS addresses and one elected VRRP owner")
						} else {
							for _, ref := range learned {
								provider, _, _ := dnsconfig.UpstreamProvider(ref)
								owners = append(owners, serviceconfig.DNSUpstreamOwner{Source: source.Name, Provider: provider, VRRP: owner})
							}
							c.note(*upstream, "Learned DNS endpoints are available only while their VRRP owner is elected. DNS retains the earlier original protocol/election expiry; loss of ownership withdraws this upstream without stopping other DNS sources.")
						}
					}
					var bootstrap []api.StatusValueSourceSpec
					for _, ref := range us.BootstrapFrom {
						bootstrap = append(bootstrap, ref.StatusValueSourceSpec())
					}
					source.BootstrapResolverFrom = append(source.BootstrapResolverFrom, c.dnsLearnedSources(*upstream, "bootstrapFrom", bootstrap)...)
					source.BootstrapResolver = append(source.BootstrapResolver, us.Bootstrap...)
					if us.SourceInterface != "" {
						ifname := c.interfaceName(*upstream, "sourceInterface", us.SourceInterface, true)
						if source.ViaInterface != "" && source.ViaInterface != ifname {
							c.reject(forward, "upstreams", "unsupported_semantics", "one native DNS source cannot bind its upstreams to different interfaces")
						} else {
							source.ViaInterface = ifname
						}
					}
					c.target(*upstream, "services/"+c.serviceName(r, "dns")+"/dns/sources/"+source.Name)
				}
			}
			spec.Sources = append(spec.Sources, source)
			c.target(forward, "services/"+c.serviceName(r, "dns")+"/dns/sources/"+source.Name)
		}
		normalized := dnsresolver.NormalizeSpec(spec)
		// Public intent leaves private storage paths to the native compiler.
		// The old normalizer otherwise inserts one process-global database path.
		normalized.QueryLog.Path = spec.QueryLog.Path
		runtime := dnsconfig.RuntimeConfig{Resource: c.serviceName(r, "dns"), Spec: normalized}
		hasDHCP := false
		for _, zone := range zones {
			zs := zone.Spec.(api.DNSZoneSpec)
			zs.Records = append([]api.DNSZoneRecordSpec(nil), zs.Records...)
			if len(zs.OwnerRefs) > 0 {
				c.reject(zone, "ownerRefs", "unsupported_semantics", "zone owner lifecycle cannot be carried into a static runtime snapshot")
			}
			if zs.DNSSEC.Enabled || zs.DNSSEC.Mode != "" {
				c.reject(zone, "dnssec", "unsupported_semantics", "native local zones do not sign or validate DNSSEC and will not assert unverified AD")
			}
			hasDHCP = hasDHCP || len(zs.DHCPDerived.Sources) > 0 || zs.DHCPDerived.LeaseFile != ""
			zs.DHCPDerived = c.dnsDHCPDerived(zone, zs.DHCPDerived)
			for i := range zs.Records {
				record := &zs.Records[i]
				path := "records[" + strconv.Itoa(i) + "]"
				if record.IPv4From.Resource == "" && (record.IPv4From.Field != "" || record.IPv4From.Optional) ||
					record.IPv6From.Resource == "" && (record.IPv6From.Field != "" || record.IPv6From.Optional) {
					c.reject(zone, path, "required", "an address source must name its resource")
				}
				if record.IPv4Source.Field != "" || record.IPv4Source.Optional || record.IPv6Source.Field != "" || record.IPv6Source.Optional {
					c.reject(zone, path, "unsupported_semantics", "deprecated record source expressions are not interpreted")
				}
				if record.IPv4From.Resource != "" {
					if record.IPv4 != "" {
						c.reject(zone, path+".ipv4From", "unsupported_semantics", "literal plus status-source fallback must be made explicit before migration")
					}
					record.IPv4, record.IPv4From = c.dnsRecordAddress(zone, path+".ipv4From", record.IPv4From, true)
				}
				if record.IPv6From.Resource != "" {
					if record.IPv6 != "" {
						c.reject(zone, path+".ipv6From", "unsupported_semantics", "literal plus status-source fallback must be made explicit before migration")
					}
					record.IPv6, record.IPv6From = c.dnsRecordAddress(zone, path+".ipv6From", record.IPv6From, false)
				}
			}
			runtime.Zones = append(runtime.Zones, dnsconfig.RuntimeZone{Name: zone.Metadata.Name, Spec: zs})
			c.target(zone, "services/"+runtime.Resource+"/dns/zones/"+zone.Metadata.Name)
		}
		if hasDHCP {
			policy, ok := c.options.DHCPDNS[r.Kind+"/"+r.Metadata.Name]
			if !ok {
				c.reject(r, "", "explicit_policy_required", "DHCP-derived DNS requires an explicit dhcp_dns policy replacing global legacy lease/sticky events with source-scoped finite lease ownership")
			} else {
				c.usePolicy(r, "dhcp_dns")
				if policy.SourceScopedFiniteLeases == nil || !*policy.SourceScopedFiniteLeases {
					c.policyIssue(r, "dhcp_dns", "source_scoped_finite_leases", "explicit_policy_required", "the DHCP DNS replacement must explicitly enable source-scoped finite lease ownership")
				}
				c.note(r, "Explicit DHCP DNS replacement: only leases from each zone's named native DHCP sources are published. Producer loss or lease expiry removes records; global lease events, sticky host recovery and lease-file import are not retained.")
			}
		}
		service := serviceconfig.Service{Name: runtime.Resource, DNS: &serviceconfig.DNS{RuntimeConfig: runtime, UpstreamOwners: owners}}
		if runtime.Spec.QueryLog.Enabled {
			service.DNS.QueryRetention = c.queryRetention
		}
		services := append(append([]serviceconfig.Service(nil), c.document.Services...), service)
		if err := serviceconfig.Validate(services); err != nil {
			c.reject(r, "", "native_validation", err.Error())
		}
		c.addService(r, service)
	}
	for _, r := range forwarders {
		if !usedForwarders[r.ID()] {
			c.reject(r, "resolver", "reference", "forwarder has no compiled resolver")
		}
	}
	for name, r := range upstreams {
		if !usedUpstreams[name] {
			c.reject(r, "", "unreferenced_resource", "upstream is not referenced by a compiled forwarder and would disappear from native intent")
		}
	}
	if len(resolvers) == 0 {
		for _, r := range zones {
			c.reject(r, "", "unreferenced_resource", "zone has no compiled DNS resolver")
		}
	}
}

func (c *compiler) dnsDHCPDerived(zone resource, spec api.DNSZoneDHCPDerivedSpec) api.DNSZoneDHCPDerivedSpec {
	if spec.LeaseFile != "" {
		if len(spec.Sources) == 0 {
			c.reject(zone, "dhcpDerived.leaseFile", "unsupported_semantics", "a legacy lease file without named DHCP producers cannot become source-owned observations")
		} else {
			c.note(zone, "The named DHCP producers replace the legacy leaseFile through the resolver's explicit finite-source policy. The old path is not read or retained; released, expired or unavailable bindings do not reappear from sticky-host files.")
			spec.LeaseFile = ""
		}
	}
	if spec.DDNS {
		// routerd's active resolver never consulted this flag or accepted DNS
		// UPDATE. Preserve DHCP lease publication without inventing a DDNS service.
		c.note(zone, "The old ddns flag did not enable DNS UPDATE or change DHCP lease publication. Named DHCP producers still publish A/PTR records; no DNS UPDATE listener is added.")
	}
	if spec.HostnameSuffix != "" {
		c.reject(zone, "dhcpDerived.hostnameSuffix", "unsupported_semantics", "the deployed resolver does not implement hostnameSuffix; DHCP names use the declared DNS zone")
	}
	if len(spec.Sources) == 0 && spec.TTL != 0 {
		c.reject(zone, "dhcpDerived.ttl", "unreferenced_resource", "DHCP-derived TTL requires a declared lease producer")
	}
	var resolved []string
	seen := map[string]bool{}
	for i, ref := range spec.Sources {
		path := "dhcpDerived.sources[" + strconv.Itoa(i) + "]"
		if strings.HasPrefix(ref, "DHCPv6Server/") {
			server := c.getRef(zone, path, ref, "DHCPv6Server")
			if server == nil {
				continue
			}
			if mode := server.Spec.(api.DHCPv6ServerSpec).Mode; mode != "" && mode != "stateless" {
				c.reject(zone, path, "unsupported_semantics", "address-allocating DHCPv6 DNS sources are not implemented")
				continue
			}
			c.note(zone, "The stateless DHCPv6 source allocates no address leases and therefore contributes no DHCP-derived A/AAAA/PTR records. SLAAC addresses are not inferred from this reference.")
			continue
		}
		server := c.getRef(zone, path, ref, "DHCPv4Server")
		if server == nil {
			continue
		}
		name := c.serviceName(*server, "dhcp")
		found := false
		for _, service := range c.document.Services {
			found = found || service.Name == name && service.DHCPServer != nil
		}
		if !found || seen[name] {
			c.reject(zone, path, "reference", "lease sources must name distinct successfully compiled native DHCPv4Server resources")
			continue
		}
		seen[name] = true
		resolved = append(resolved, "DHCPv4Server/"+name)
	}
	if len(spec.Sources) != 0 {
		c.note(zone, "Declared DHCPv4Server references become native lease-source identities; active lease records remain dynamic and are never frozen into static DNS records.")
	}
	spec.Sources = resolved
	return spec
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}

func (c *compiler) dnsStaticAddress(r resource, path string, source api.StatusValueSourceSpec) string {
	ref := c.byRef[source.Resource]
	if ref == nil {
		c.reject(r, path, "reference", "listener source must name an explicit static address resource")
		return ""
	}
	ipv4 := true
	if value, ok := ref.Spec.(api.VirtualAddressSpec); ok {
		ipv4 = value.Family != "ipv6"
	}
	return c.resolveStaticAddress(r, path, source, ipv4)
}

func (c *compiler) dnsUpstreamURL(r resource, s api.DNSUpstreamSpec) string {
	address := strings.TrimSpace(s.Address)
	if !validServerName(address) {
		c.reject(r, "address", "invalid_value", "upstream address must be an IP address or DNS hostname without URL credentials")
		return ""
	}
	if s.Port < 0 || s.Port > 65535 {
		c.reject(r, "port", "invalid_value", "upstream port must be 1..65535, or omitted for its protocol default")
		return ""
	}
	protocol := strings.ToLower(strings.TrimSpace(s.Protocol))
	if protocol == "" {
		protocol = "udp"
	}
	port := s.Port
	query := url.Values{}
	if s.TLSName != "" {
		if !validServerName(s.TLSName) {
			c.reject(r, "tlsName", "invalid_value", "TLS name must be a certificate hostname or IP address")
			return ""
		}
		query.Set("serverName", s.TLSName)
	}
	raw := ""
	switch protocol {
	case "udp", "tcp":
		if s.TLSName != "" || s.Path != "" {
			c.reject(r, "", "unsupported_semantics", "TLS name and HTTP path have no meaning for an unencrypted DNS transport")
		}
		if port == 0 {
			port = 53
		}
		raw = protocol + "://" + net.JoinHostPort(address, strconv.Itoa(port))
	case "dot":
		if s.Path != "" {
			c.reject(r, "path", "unsupported_semantics", "HTTP path cannot be applied to DNS over TLS")
		}
		if port == 0 {
			port = 853
		}
		raw = "tls://" + net.JoinHostPort(address, strconv.Itoa(port))
	case "doh":
		if port == 0 {
			port = 443
		}
		path := first(strings.TrimSpace(s.Path), "/dns-query")
		if strings.ContainsAny(path, "?#\r\n") {
			c.reject(r, "path", "invalid_value", "HTTP path must not include a query, fragment or newline")
			return ""
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		raw = "https://" + net.JoinHostPort(address, strconv.Itoa(port)) + path
	default:
		c.reject(r, "protocol", "invalid_value", "the pinned public resource schema supports udp, tcp, dot and doh")
		return ""
	}
	if encoded := query.Encode(); encoded != "" && (protocol == "dot" || protocol == "doh") {
		raw += "?" + encoded
	}
	return raw
}

func validServerName(value string) bool {
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Zone() == "" && !addr.IsUnspecified() && !addr.IsMulticast()
	}
	// DNS wire-label validation alone permits characters such as @ and :.
	// They must not turn an address into URL userinfo, a port, or a query.
	return dnsconfig.ValidHostname(value) && !strings.ContainsAny(value, "@/\\?#[]:%")
}

// Translate producer identities once; live endpoints and their original
// deadlines continue through the same DNS input as native configuration.
func (c *compiler) dnsLearnedSources(r resource, path string, sources []api.StatusValueSourceSpec) []api.StatusValueSourceSpec {
	var result []api.StatusValueSourceSpec
	for i, source := range sources {
		field := path + "[" + strconv.Itoa(i) + "]"
		if source.Field != "" && source.Field != "dnsServers" {
			c.reject(r, field, "reference", "DNS sources select dnsServers")
			continue
		}
		provider := c.byRef[source.Resource]
		if provider == nil {
			c.reject(r, field, "reference", "DNS source must identify a configured producer")
			continue
		}
		var name string
		switch provider.Kind {
		case "DHCPv6PrefixDelegation", "DHCPv6Information":
			if index, ok := c.dhcp6Source(r, field, source.Resource); ok {
				name = c.document.Services[index].Name
			}
		case "IPv6RAAddress":
			if index, ok := c.raClients[source.Resource]; ok {
				name = c.document.Services[index].Name
			}
		case "DHCPv4Client":
			name = c.serviceName(*provider, "dhcp4")
		case "TailscaleNode":
			name = c.serviceName(*provider, "tail")
		default:
			c.reject(r, field, "unsupported_semantics", "DNS source requires a compiled DHCP, RA or Tailscale producer")
		}
		if name != "" {
			result = append(result, api.StatusValueSourceSpec{Resource: provider.Kind + "/" + name, Field: "dnsServers", Optional: source.Optional})
		}
	}
	return result
}

// Keep dynamic address ownership in DNS intent; observations never become literals.
func (c *compiler) dnsRecordAddress(r resource, path string, source api.StatusValueSourceSpec, ipv4 bool) (string, api.StatusValueSourceSpec) {
	ref := c.byRef[source.Resource]
	if ref != nil && !source.Optional && (source.Field == "" || source.Field == "address") {
		if ref.Kind == "VirtualAddress" && ref.Spec.(api.VirtualAddressSpec).Mode == "vrrp" {
			return "", api.StatusValueSourceSpec{Resource: "VirtualAddress/" + c.serviceName(*ref, "vrrp"), Field: "address"}
		}
		if ref.Kind == "IPv6DelegatedAddress" && !ipv4 {
			spec := ref.Spec.(api.IPv6DelegatedAddressSpec)
			provider := c.getRef(*ref, "prefixDelegation", spec.PrefixDelegation, "DHCPv6PrefixDelegation")
			if provider == nil {
				return "", api.StatusValueSourceSpec{}
			}
			if index, ok := c.pd[provider.Kind+"/"+provider.Metadata.Name]; ok {
				return "", api.StatusValueSourceSpec{Resource: "IPv6DelegatedAddress/" + c.document.Services[index].Name + "/" + c.serviceName(*ref, "lan"), Field: "address"}
			}
		}
	}
	return c.resolveStaticAddress(r, path, source, ipv4), api.StatusValueSourceSpec{}
}
