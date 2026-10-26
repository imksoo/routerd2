// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// The active Linux controller in the source pin differs from the FreeBSD/runtime
// helper: it uses AFTRFrom, FQDN, IPv6 and RemoteAddress in that order, the local
// DNS listener and first AAAA selection, and retains a pending tunnel indefinitely.
// Every conversion to the native lifetime/ownership model requires policy.
func (c *compiler) dynamicDSLite(r resource, policy DSLitePolicy) {
	c.allow(r, "interface", "tunnelName", "enabled", "aftrFQDN", "aftrIPv6", "aftrDNSServers", "aftrAddressOrdinal", "aftrAddressSelection", "remoteAddress", "localAddress", "localAddressFrom", "localIPv6Source", "aftrFrom", "aftrSource", "localAddressSource", "localDelegatedAddress", "localAddressSuffix", "defaultRoute", "routeMetric", "mtu", "encapsulationLimit")
	c.usePolicy(r, "ds_lite")
	s := r.Spec.(api.DSLiteTunnelSpec)
	if s.Enabled != nil && !*s.Enabled {
		c.reject(r, "enabled", "unsupported_semantics", "disabled legacy tunnels cannot be activated by migration policy")
	}
	if s.DefaultRoute || s.RouteMetric != 0 {
		c.reject(r, "defaultRoute", "unsupported_semantics", "the pinned Linux tunnel controller ignores defaultRoute/routeMetric; declare an independent IPv4Route instead of activating ineffective legacy fields")
	}
	if s.EncapsulationLimit != "" && s.EncapsulationLimit != "none" {
		c.reject(r, "encapsulationLimit", "unsupported_semantics", "native DS-Lite suppresses the IPv6 encapsulation-limit option")
	}
	if s.AFTRSource != "" {
		c.reject(r, "aftrSource", "unsupported_semantics", "this removed status-expression field cannot replace an explicit aftrFrom reference")
	}
	underlay := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(r, "interface", underlay)
	name := c.aliases[r.Kind+"/"+r.Metadata.Name]
	if _, exists := c.dynamicTunnels[name]; exists || c.interfaces[name] != nil {
		c.reject(r, "tunnelName", "ownership_conflict", "a dynamic tunnel cannot share its kernel interface with another static or dynamic owner")
	}
	mtu := s.MTU
	if mtu == 0 {
		mtu = 1460
	}
	if s.MTU != 0 && (s.MTU < 1280 || s.MTU > 65495) {
		c.reject(r, "mtu", "invalid_value", "MTU must satisfy both the pinned DSLiteTunnel range and the native IPv6 encapsulation limit")
	}
	inner := "192.0.0.2"
	if meaningful(child(r.node, "localAddressFrom")) {
		inner = c.dsliteInnerAddress(r, s.LocalAddressFrom)
	}
	ip, err := netip.ParseAddr(inner)
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
		c.reject(r, "localAddressFrom", "invalid_value", "inner endpoint must resolve to a usable static IPv4 unicast address")
	}
	noRoute := false
	intent := serviceconfig.DSLite{Interface: underlay, Tunnel: name,
		Selection: policy.Selection, Ordinal: policy.Ordinal,
		TimeoutSeconds: policy.TimeoutSeconds, RetrySeconds: policy.RetrySeconds,
		MTU: mtu, B4Address: inner + "/32", B4Peer: "192.0.0.1/32", DefaultRoute: &noRoute,
		Resolvers: append([]serviceconfig.AFTRResolver(nil), policy.Resolvers...),
	}
	c.dynamicDSLiteSource(r, s, policy, &intent)

	if meaningful(child(r.node, "aftrFrom")) {
		if s.AFTRFrom.Field != "aftrName" || s.AFTRFrom.Optional {
			c.reject(r, "aftrFrom", "unsupported_semantics", "AFTR input must explicitly select non-optional aftrName from a compiled PD/information resource; phase/default and arbitrary status fields are not candidates")
		}
		if index, ok := c.dhcp6Source(r, "aftrFrom.resource", s.AFTRFrom.Resource); ok {
			provider := c.document.Services[index]
			if provider.Interface() != underlay {
				c.reject(r, "aftrFrom", "reference_scope", "AFTR information must come from the same underlay interface as the tunnel")
			}
			intent.AFTRFrom = provider.Name
		}
	}
	for _, candidate := range []struct{ field, value string }{{"aftrFQDN", s.AFTRFQDN}, {"aftrIPv6", s.AFTRIPv6}, {"remoteAddress", s.RemoteAddress}} {
		if candidate.value == "" {
			continue
		}
		value := strings.TrimSpace(candidate.value)
		_, literalErr := netip.ParseAddr(value)
		if candidate.field != "aftrFQDN" || literalErr == nil {
			address, ok := globalIPv6(value)
			if !ok {
				c.reject(r, candidate.field, "invalid_value", "literal AFTR candidates must be IPv6 global or unique-local unicast addresses")
			} else {
				value = address
			}
		}
		intent.Candidates = append(intent.Candidates, value)
	}
	if len(intent.Candidates) == 0 && intent.AFTRFrom == "" {
		c.reject(r, "aftrFrom", "required", "at least one supported AFTR candidate is required")
	}
	seen := map[string]bool{}
	for _, ref := range policy.DNSFrom {
		index, ok := c.policyDNSProvider(r, ref)
		if !ok {
			continue
		}
		provider := c.document.Services[index]
		if provider.Interface() != underlay {
			c.policyIssue(r, "ds_lite", "dns_from", "reference_scope", "every DNS source must use the tunnel's underlay interface")
		}
		if seen[provider.Name] {
			c.policyIssue(r, "ds_lite", "dns_from", "duplicate_source", "DNS sources must identify different native services; an information alias and its PD owner are one source")
		}
		seen[provider.Name] = true
		intent.DNSFrom = append(intent.DNSFrom, provider.Name)
	}
	c.dynamicDSLitePolicy(r, s, policy, intent)
	service := serviceconfig.Service{Name: c.serviceName(r, "aftr"), DSLite: &intent}
	// Check the wire-facing values without asking the resolver or host network.
	// Cross-service ownership is checked again by config.Validate at the end.
	cfg := service.AFTRConfig()
	cfg.ConfigDigest = strings.Repeat("0", 64)
	in := aftr.Input{Resource: service.Name, Interface: underlay, ConfigDigest: cfg.ConfigDigest, Generation: 1, UpdatedAt: time.Unix(1, 0).UTC(), Active: true, Source: aftr.Source{Address: intent.LocalAddress}}
	for _, candidate := range intent.Candidates {
		in.Candidates = append(in.Candidates, aftr.Candidate{Value: candidate, Origin: "configured"})
	}
	for _, resolver := range intent.Resolvers {
		in.Resolvers = append(in.Resolvers, aftr.Resolver{Address: resolver.Address, Port: resolver.Port, Interface: underlay})
	}
	if err := aftr.ValidateInput(cfg, in); err != nil {
		c.policyIssue(r, "ds_lite", "", "policy_validation", "AFTR intent violates native candidate, IPv6 bootstrap, duplicate, source, selection or timing restrictions")
	}
	if len(intent.Candidates)+boolCount(intent.AFTRFrom != "") > aftr.MaxCandidates || len(intent.DNSFrom) > 8 {
		c.policyIssue(r, "ds_lite", "", "policy_limit", "native AFTR permits four total candidate sources and eight learned DNS sources")
	}
	c.dynamicTunnels[name] = len(c.document.Services)
	c.addService(r, service)
	c.note(r, "Explicit migration policy replaces local-resolver lookup and indefinite pending-tunnel retention with scoped IPv6 DNS, finite DNS/dependency expiry, and withdrawal when the owned source is no longer preferred; this is deliberate new native intent, not automatic legacy parity.")
	c.note(r, "AFTR candidate precedence remains information aftrName, aftrFQDN, aftrIPv6, remoteAddress. The B4 /32 peer 192.0.0.1/32 and fixed 1460-byte default MTU are retained; only independent route resources can authorize a default route.")
}

// The pinned configuration validator requires an explicit source field. Its
// controller has a historical static-address fallback for an omitted field,
// but relying on that fallback would accept a configuration the original
// validator rejects. The migration boundary keeps the public contract.
func (c *compiler) dsliteInnerAddress(r resource, source api.StatusValueSourceSpec) string {
	if source.Field != "address" {
		c.reject(r, "localAddressFrom.field", "unsupported_semantics", "inner IPv4 source must explicitly select address, as required by the pinned configuration validator; controller-only fallback behavior is not imported")
		return ""
	}
	return c.resolveStaticAddress(r, "localAddressFrom", source, true)
}

func (c *compiler) dynamicDSLiteSource(r resource, s api.DSLiteTunnelSpec, policy DSLitePolicy, out *serviceconfig.DSLite) {
	if s.LocalAddressSource == "delegatedAddress" {
		ref := c.getRef(r, "localDelegatedAddress", s.LocalDelegatedAddress, "IPv6DelegatedAddress")
		if ref == nil {
			return
		}
		delegated := ref.Spec.(api.IPv6DelegatedAddressSpec)
		provider := c.getRef(r, "localDelegatedAddress", delegated.PrefixDelegation, "DHCPv6PrefixDelegation")
		if provider == nil {
			return
		}
		id := provider.Kind + "/" + provider.Metadata.Name
		index, ok := c.pd[id]
		if !ok {
			c.reject(r, "localDelegatedAddress", "reference", "delegated source provider was not compiled")
			return
		}
		if policy.SourceFrom != "" && policy.SourceFrom != id {
			c.policyIssue(r, "ds_lite", "source_from", "policy_source", "delegated source policy must retain the declared PD provider")
			return
		}
		service := c.document.Services[index]
		if service.Interface() != out.Interface {
			c.reject(r, "localDelegatedAddress", "reference_scope", "delegated source must belong to this tunnel's WAN provider")
			return
		}
		name := c.serviceName(*ref, "lan")
		for _, lan := range service.DHCP6.Delegations {
			if lan.Name != name {
				continue
			}
			if s.LocalAddressSuffix != "" {
				a, err := netip.ParseAddr(s.LocalAddressSuffix)
				if err != nil || !a.Is6() || a.Is4In6() {
					c.reject(r, "localAddressSuffix", "invalid_value", "expected an IPv6 host suffix")
					return
				}
				raw := a.As16()
				host := binary.BigEndian.Uint64(raw[8:])
				if binary.BigEndian.Uint64(raw[:8]) != 0 || host == 0 {
					c.reject(r, "localAddressSuffix", "unsupported_semantics", "delegated source requires a nonzero 64-bit host suffix")
					return
				}
				if host != lan.HostID {
					lan = serviceconfig.Delegation{Name: c.serviceName(r, "b4-source"), Interface: lan.Interface, SubnetID: lan.SubnetID, HostID: host}
					service.DHCP6.Delegations = append(service.DHCP6.Delegations, lan)
					c.target(r, "services/"+service.Name+"/delegations/"+lan.Name)
				}
			}
			out.SourceFrom, out.SourceDelegation = service.Name, lan.Name
			c.note(r, "The declared PD provider and LAN allocation own the DS-Lite source and its original lifetimes. A different source suffix creates another managed address on that LAN, without another DHCP socket or address-selection daemon; native delegation addresses use /64.")
			return
		}
		c.reject(r, "localDelegatedAddress", "reference", "delegated LAN allocation was not compiled")
		return
	}
	if s.LocalAddressSource != "" && s.LocalAddressSource != "interface" && s.LocalAddressSource != "static" || s.LocalDelegatedAddress != "" || s.LocalAddressSuffix != "" {
		c.reject(r, "localAddressSource", "unsupported_semantics", "unsupported source mode, interface suffix or HA/VMAC source; delegated sources require an explicit localDelegatedAddress")
		return
	}
	value := s.LocalAddress
	if s.LocalIPv6Source != "" {
		if s.LocalAddressSource == "static" || value != "" {
			c.reject(r, "localIPv6Source", "unsupported_semantics", "multiple local literals/status fallbacks or an ignored static-mode source cannot be collapsed into one native source")
			return
		}
		value = s.LocalIPv6Source
	}
	if value != "" {
		if address, ok := globalIPv6(value); ok {
			if policy.SourceFrom != "" {
				c.policyIssue(r, "ds_lite", "source_from", "policy_source", "a literal legacy outer source must remain that literal; source_from cannot override it")
			}
			out.LocalAddress = address
			if !c.ownsStaticAddress(out.Interface, address) {
				c.reject(r, "localAddress", "source_ownership", "literal outer IPv6 must be declared by an unconditional migrated static address on this underlay; observed host addresses are not adopted")
			}
			return
		}
		// Only this explicit legacy RA status expression can be bound to the
		// matching replacement policy. Arbitrary status evaluation is forbidden.
		ref := strings.TrimSuffix(strings.TrimPrefix(value, "${"), ".status.address}")
		if s.LocalIPv6Source == "" || value != "${"+ref+".status.address}" || ref != policy.SourceFrom {
			c.reject(r, "localIPv6Source", "unsupported_semantics", "only a canonical IPv6RAAddress status.address expression matching policy source_from is supported; arbitrary source expressions are not frozen")
			return
		}
	}
	if s.LocalAddressSource == "static" {
		c.reject(r, "localAddress", "required", "static outer source requires an explicit usable IPv6 literal")
		return
	}
	index, ok := c.raClients[policy.SourceFrom]
	if !ok || !strings.HasPrefix(policy.SourceFrom, "IPv6RAAddress/") {
		c.policyIssue(r, "ds_lite", "source_from", "policy_reference", "discovered outer source requires the exact identity of a compiled IPv6RAAddress policy")
		return
	}
	provider := c.document.Services[index]
	if provider.Interface() != out.Interface || provider.RAClient.SLAAC == nil || !*provider.RAClient.SLAAC {
		c.policyIssue(r, "ds_lite", "source_from", "reference_scope", "outer source requires SLAAC enabled on the same underlay interface")
	}
	out.SourceFrom = provider.Name
	c.note(r, "Source policy replaces first usable kernel address discovery with the named RA client's lexically selected owned, DAD-complete, preferred global address and its absolute lifetimes; external, temporary, PD-derived and HA-staged addresses are not adopted.")
}

func (c *compiler) dynamicDSLitePolicy(r resource, s api.DSLiteTunnelSpec, policy DSLitePolicy, intent serviceconfig.DSLite) {
	if policy.Selection != "first" && policy.Selection != "ordinal" && policy.Selection != "ordinalModulo" {
		c.policyIssue(r, "ds_lite", "selection", "policy_required", "select first, ordinal or ordinalModulo explicitly; the active Linux controller used first answer, whereas other legacy renderers differed")
	}
	if (policy.Selection == "ordinal" || policy.Selection == "ordinalModulo") && policy.Ordinal <= 0 {
		c.policyIssue(r, "ds_lite", "ordinal", "policy_required", "ordinal policies require an explicit positive address ordinal")
	}
	if policy.TimeoutSeconds <= 0 || policy.RetrySeconds <= 0 {
		c.policyIssue(r, "ds_lite", "", "policy_required", "positive timeout_seconds and retry_seconds must explicitly select native resolution and retry timing")
	}
	if s.AFTRAddressOrdinal != 0 || s.AFTRAddressSelection != "" {
		selection := first(s.AFTRAddressSelection, "ordinal")
		ordinal := s.AFTRAddressOrdinal
		if ordinal == 0 {
			ordinal = 1
		}
		if ordinal < 1 || selection != "ordinal" && selection != "ordinalModulo" {
			c.reject(r, "aftrAddressSelection", "invalid_value", "legacy AFTR ordinal/selection fields must be valid before migration")
		} else if policy.Selection != selection || policy.Ordinal != ordinal {
			c.reject(r, "aftrAddressSelection", "policy_conflict", "legacy ordinal fields were ignored by the Linux controller; policy must explicitly repeat them to activate that intent, or the legacy fields must be removed after review")
		}
		c.note(r, "Legacy aftrAddressOrdinal/aftrAddressSelection were ignored by the active Linux controller. The repeated explicit policy activates sorted-address ordinal selection and therefore intentionally changes its first-answer behavior.")
	} else if policy.Selection != "first" {
		c.note(r, "Explicit ordinal selection intentionally changes the pinned Linux first-AAAA behavior to canonical sorted-address selection.")
	}
	if len(s.AFTRDNSServers) != 0 {
		same := len(s.AFTRDNSServers) == len(policy.Resolvers)
		for i, value := range s.AFTRDNSServers {
			address, err := netip.ParseAddr(value)
			if err != nil || !address.Is6() || address.Is4In6() || i >= len(policy.Resolvers) || address.String() != policy.Resolvers[i].Address || policy.Resolvers[i].Port != 0 && policy.Resolvers[i].Port != 53 {
				same = false
			}
		}
		if !same {
			c.reject(r, "aftrDNSServers", "policy_conflict", "the active Linux controller ignored aftrDNSServers; policy must explicitly repeat a supported IPv6 server list on port 53, or those legacy fields must be resolved before migration")
		}
		c.note(r, "Legacy aftrDNSServers was ignored by the active Linux controller. Repeating it as explicit IPv6 bootstrap policy intentionally activates direct underlay DNS instead of the process-local resolver.")
	}
	needsDNS := intent.AFTRFrom != ""
	for _, value := range intent.Candidates {
		if _, err := netip.ParseAddr(value); err != nil {
			needsDNS = true
		}
	}
	if needsDNS && len(intent.Resolvers) == 0 && len(intent.DNSFrom) == 0 {
		c.policyIssue(r, "ds_lite", "resolvers", "bootstrap_required", "hostname and learned AFTR candidates require explicit underlay IPv6 resolvers or RA/PD DNS sources; no host/local resolver fallback is inferred")
	}
	for _, resolver := range intent.Resolvers {
		if c.ownsStaticAddress("", resolver.Address) {
			c.policyIssue(r, "ds_lite", "resolvers", "bootstrap_cycle", "an AFTR bootstrap resolver cannot be an address owned by this router; use an independent underlay DNS server or RA/PD DNS source")
		}
	}
}

func (c *compiler) dhcp6Source(r resource, field, ref string) (int, bool) {
	if index, ok := c.pd[ref]; ok && strings.HasPrefix(ref, "DHCPv6PrefixDelegation/") {
		return index, true
	}
	if index, ok := c.information[ref]; ok && strings.HasPrefix(ref, "DHCPv6Information/") {
		return index, true
	}
	c.reject(r, field, "reference", "source must identify an existing compiled PD or supplementary information resource using exact Kind/name")
	return 0, false
}

func (c *compiler) policyDNSProvider(r resource, ref string) (int, bool) {
	if index, ok := c.raClients[ref]; ok && strings.HasPrefix(ref, "IPv6RAAddress/") {
		if dns := c.document.Services[index].RAClient.DNS; dns == nil || !*dns {
			c.policyIssue(r, "ds_lite", "dns_from", "policy_source", "an RA DNS source must enable dns in its own resource-scoped migration policy")
		}
		return index, true
	}
	if index, ok := c.pd[ref]; ok && strings.HasPrefix(ref, "DHCPv6PrefixDelegation/") {
		return index, true
	}
	if index, ok := c.information[ref]; ok && strings.HasPrefix(ref, "DHCPv6Information/") {
		return index, true
	}
	c.policyIssue(r, "ds_lite", "dns_from", "policy_reference", "DNS dependency must be an exact compiled RA/PD/information identity; DNS services and arbitrary status paths are not bootstrap sources")
	return 0, false
}

func (c *compiler) ownsStaticAddress(ifname, address string) bool {
	for name, iface := range c.interfaces {
		if ifname != "" && name != ifname {
			continue
		}
		for _, value := range iface.Addresses {
			p, err := netip.ParsePrefix(value)
			if err == nil && p.Addr().String() == address {
				return true
			}
		}
	}
	return false
}

func globalIPv6(value string) (string, bool) {
	a, err := netip.ParseAddr(value)
	return a.String(), err == nil && a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && a.Zone() == ""
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (c *compiler) dynamicDSLiteRoute(r resource, index int, prefix netip.Prefix, gateway string, metric, family int) {
	service := &c.document.Services[index]
	intent := service.DSLite
	if family != 4 || prefix.String() != "0.0.0.0/0" || gateway != "" && gateway != "192.0.0.1" {
		c.reject(r, "destination", "unsupported_semantics", "a dynamically owned DS-Lite tunnel currently accepts one IPv4 default route, optionally via its retained 192.0.0.1 peer; other routes require additional native lifetime ownership")
		return
	}
	if c.dynamicRoutes[intent.Tunnel] {
		c.reject(r, "destination", "route_owner", "multiple route resources cannot own the one native DS-Lite default route")
		return
	}
	if metric < 0 || uint64(metric) > 1<<32-1 {
		c.reject(r, "metric", "invalid_value", "route metric must fit a nonnegative native route metric")
		return
	}
	c.dynamicRoutes[intent.Tunnel] = true
	yes := true
	intent.DefaultRoute, intent.RouteGateway, intent.Metric = &yes, gateway, metric
	c.target(r, "services/"+service.Name+"/ds_lite/default_route")
	c.note(r, "Independent default-route intent is attached to the dynamically owned B4 service, retaining gateway and metric; it is withdrawn with the AFTR/source lifetime instead of keeping stale tunnel ownership.")
}
