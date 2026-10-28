// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) dhcp4(r resource) {
	c.allow(r, "interface", "hostname", "requestedAddress", "classID", "clientID", "useRoutes", "useDNS", "routeMetric")
	s := r.Spec.(api.DHCPv4ClientSpec)
	if s.UseDNS == nil || *s.UseDNS {
		c.reject(r, "useDNS", "unsupported_semantics", "legacy useDNS defaults to true and updates the host resolver; native learned DNS is status only, so useDNS:false is required")
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(r, "interface", name)
	c.addService(r, serviceconfig.Service{
		Name: c.serviceName(r, "dhcp4"),
		DHCP4: &serviceconfig.DHCP4{
			Interface: name, Hostname: s.Hostname, RequestedAddress: s.RequestedAddress,
			ClassID: s.ClassID, ClientID: s.ClientID, DefaultRoute: s.UseRoutes, Metric: s.RouteMetric,
		},
	})
	c.note(r, "DHCP ownership starts from a new native journal; runtime lease files are not imported.")
}

func (c *compiler) dhcp6(r resource) string {
	c.allow(r, "interface", "client", "profile", "prefixLength", "clientDUID", "required")
	s := r.Spec.(api.DHCPv6PrefixDelegationSpec)
	if s.Client != "" && s.Client != "routerd" {
		c.reject(r, "client", "unsupported_semantics", "an explicitly selected external DHCPv6 client is not silently replaced")
	}
	switch s.Profile {
	case "", api.IPv6PDProfileDefault, api.IPv6PDProfileNTTNGNDirectHikariDenwa, api.IPv6PDProfileNTTHGWLANPD:
	default:
		c.reject(r, "profile", "invalid_value", "unknown provider profile")
	}
	if s.Required {
		c.reject(r, "required", "unsupported_semantics", "legacy required-prefix readiness policy is not represented by native service readiness")
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(r, "interface", name)
	service := serviceconfig.Service{
		Name: c.serviceName(r, "pd"),
		DHCP6: &serviceconfig.DHCP6{RequestPrefix: true,
			Interface: name, ClientDUID: s.ClientDUID, IAID: 1,
			PrefixLength: api.EffectiveIPv6PDPrefixLength(s.Profile, s.PrefixLength),
		},
	}
	c.pd[r.Kind+"/"+r.Metadata.Name] = len(c.document.Services)
	c.addService(r, service)
	c.note(r, "DUID-LL and IAID 1 defaults are retained; historical leases and resource status are not imported.")
	return service.Name
}

func (c *compiler) raClient(r resource) {
	c.allow(r, "interface", "managed", "required")
	s := r.Spec.(api.IPv6RAAddressSpec)
	policy, exists := c.options.RAClients[r.Kind+"/"+r.Metadata.Name]
	if !exists {
		c.reject(r, "", "unsupported_semantics", "legacy managed RA delegates SLAAC, route metrics, DNS, MTU and DHCPv6 triggers to host-network-manager defaults; an explicit resource-scoped ra_clients migration policy is required")
		return
	}
	c.usePolicy(r, "ra_clients")
	if s.Managed != nil && !*s.Managed {
		c.reject(r, "managed", "unsupported_semantics", "an observe-only RA resource cannot become a native socket/address/route owner through migration policy")
	}
	if s.Required {
		c.reject(r, "required", "unsupported_semantics", "legacy required-address readiness is not represented by native socket readiness")
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(r, "interface", name)
	if policy.Interface != name {
		c.policyIssue(r, "ra_clients", "interface", "policy_interface", "RA policy interface must equal the kernel interface resolved from this legacy resource")
	}
	if policy.SLAAC == nil || policy.DefaultRoute == nil || policy.OnLink == nil || policy.RouteInformation == nil || policy.DNS == nil {
		c.policyIssue(r, "ra_clients", "", "policy_required", "RA policy must explicitly set slaac, default_route, on_link, route_information and dns; OS defaults are not inferred")
	}
	if policy.Table <= 0 || policy.Metric <= 0 {
		c.policyIssue(r, "ra_clients", "", "policy_required", "RA policy must explicitly select positive table and metric values; host route priorities are not inferred")
	}
	service := serviceconfig.Service{Name: c.serviceName(r, "ra-wan"), RAClient: &policy}
	if err := serviceconfig.Validate([]serviceconfig.Service{service}); err != nil {
		c.policyIssue(r, "ra_clients", "", "policy_validation", "RA policy does not satisfy the native address, interface-ID, router-filter or route restrictions")
	}
	for _, index := range c.raClients {
		if c.document.Services[index].Interface() == name {
			c.reject(r, "interface", "socket_owner", "one native RA client must own each interface; multiple legacy RA resources cannot acquire independent sockets there")
		}
	}
	c.raClients[r.Kind+"/"+r.Metadata.Name] = len(c.document.Services)
	c.addService(r, service)
	c.note(r, "Explicit migration policy replaces host-network-manager RA defaults with native SLAAC, default/on-link/RIO route, DNS and route-metric/table intent; this is a deliberate behavior change, not automatic legacy parity.")
	c.note(r, "The sole network writer disables kernel RA/autoconf before the native receiver starts. Learned DNS remains bounded typed state; it does not update resolv.conf. Native use_mtu/use_hop_limit and DHCPv6 ra_from are explicit optional policies; this import does not infer them from host-network-manager defaults.")
	c.note(r, "An omitted native interface_id uses EUI-64; an omitted source_address selects the interface link-local address. Required-address readiness and legacy observed addresses are not imported.")
}

func (c *compiler) dhcp6Information(r resource) {
	c.allow(r, "interface", "request")
	s := r.Spec.(api.DHCPv6InformationSpec)
	if len(s.Request) != 0 {
		c.reject(r, "request", "unsupported_semantics", "the pinned Linux information controller ignores request and always exposes its PD client's fixed option set; nonempty selection intent must be resolved explicitly before migration")
	}
	c.interfaceName(r, "interface", s.Interface, false)
	var providers []int
	for _, candidate := range c.resources {
		pd, ok := candidate.Spec.(api.DHCPv6PrefixDelegationSpec)
		if !ok || pd.Interface != s.Interface {
			continue
		}
		if index, ok := c.pd[candidate.Kind+"/"+candidate.Metadata.Name]; ok {
			providers = append(providers, index)
		}
	}
	if len(providers) != 1 {
		c.reject(r, "interface", "information_provider", "information requires exactly one compiled PD resource with the same literal interface reference, matching the pinned Linux controller; standalone information and ambiguous PD ownership are not activated")
		return
	}
	index := providers[0]
	c.information[r.Kind+"/"+r.Metadata.Name] = index
	c.target(r, "services/"+c.document.Services[index].Name)
	c.note(r, "Information is an alias of the matching PD service: one DUID/IAID identity and one UDP 546 socket perform IA_PD and supplementary Information-Request; no second information-only service is created.")
	c.note(r, "DNS, domain-search, SNTP and AFTR options retain the fixed client request set. Native freshness additionally requests and obeys the information-refresh option, bounded by the active PD lease; legacy status and lease files are not imported.")
}

func (c *compiler) delegation(r resource) {
	c.allow(r, "prefixDelegation", "prefixSource", "interface", "subnetID", "addressSuffix", "prefixLength", "sendRA", "announce")
	s := r.Spec.(api.IPv6DelegatedAddressSpec)
	if s.PrefixSource != "" {
		c.reject(r, "prefixSource", "unsupported_semantics", "deprecated status expressions must be replaced with prefixDelegation references")
	}
	if s.PrefixLength != 0 && s.PrefixLength != 64 && s.PrefixLength != 128 {
		c.reject(r, "prefixLength", "unsupported_semantics", "delegated addresses require the original /64 LAN or /128 host mask")
	}
	if s.SendRA || s.Announce {
		selected := false
		for _, policy := range c.options.DHCP6Servers {
			selected = selected || policy.DelegatedAddress == r.Kind+"/"+r.Metadata.Name
		}
		if !selected {
			c.reject(r, "sendRA", "unsupported_semantics", "RA declaration requires a stateless server policy selecting this allocation and its native lifetime intent")
		}
	}
	ref := c.getRef(r, "prefixDelegation", s.PrefixDelegation, "DHCPv6PrefixDelegation")
	if ref == nil {
		return
	}
	index, ok := c.pd[ref.Kind+"/"+ref.Metadata.Name]
	if !ok {
		c.reject(r, "prefixDelegation", "reference", "prefix provider was not compiled")
		return
	}
	pd := c.document.Services[index].DHCP6
	prefixLength := pd.PrefixLength
	if prefixLength == 0 {
		prefixLength = 60 // The pinned pdclient default, also retained natively.
	}
	if prefixLength > 64 {
		c.reject(r, "prefixDelegation", "unsupported_semantics", "delegated prefixes longer than /64 cannot provide a native /64 allocation")
		return
	}
	// Original DeriveIPv6Address parses subnetID as hexadecimal, including a
	// value such as "10" without a 0x prefix. Treating it as decimal is a bug.
	subnetText := strings.TrimPrefix(strings.TrimSpace(s.SubnetID), "0x")
	if subnetText == "" {
		subnetText = "0"
	}
	subnet, err := strconv.ParseUint(subnetText, 16, 64)
	if err != nil || prefixLength > 0 && prefixLength <= 64 && subnet>>(64-prefixLength) != 0 {
		c.reject(r, "subnetID", "invalid_value", "hexadecimal subnetID must fit inside the delegated prefix without overwriting provider prefix bits")
		return
	}
	suffix, err := netip.ParseAddr(s.AddressSuffix)
	if err != nil || !suffix.Is6() || suffix.Is4In6() {
		c.reject(r, "addressSuffix", "invalid_value", "expected an IPv6 suffix")
		return
	}
	raw := suffix.As16()
	host := binary.BigEndian.Uint64(raw[8:])
	if host == 0 {
		c.reject(r, "addressSuffix", "unsupported_semantics", "zero host ID is explicit in legacy configuration but defaults to 1 in native delegation intent")
		return
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(r, "interface", name)
	delegation := serviceconfig.Delegation{Name: c.serviceName(r, "lan"), Interface: name, SubnetID: subnet, HostID: host, PrefixLength: s.PrefixLength}
	pd.Delegations = append(pd.Delegations, delegation)
	c.target(r, "services/"+c.document.Services[index].Name+"/delegations/"+delegation.Name)
}
