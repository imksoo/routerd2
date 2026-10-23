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

func (c *compiler) dhcp6(r resource) {
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
		DHCP6: &serviceconfig.DHCP6{
			Interface: name, ClientDUID: s.ClientDUID, IAID: 1,
			PrefixLength: api.EffectiveIPv6PDPrefixLength(s.Profile, s.PrefixLength),
		},
	}
	c.pd[r.Kind+"/"+r.Metadata.Name] = len(c.document.Services)
	c.addService(r, service)
	c.note(r, "DUID-LL and IAID 1 defaults are retained; historical leases and resource status are not imported.")
}

func (c *compiler) delegation(r resource) {
	c.allow(r, "prefixDelegation", "prefixSource", "interface", "subnetID", "addressSuffix", "prefixLength", "sendRA", "announce")
	s := r.Spec.(api.IPv6DelegatedAddressSpec)
	if s.PrefixSource != "" {
		c.reject(r, "prefixSource", "unsupported_semantics", "deprecated status expressions must be replaced with prefixDelegation references")
	}
	if s.PrefixLength != 0 && s.PrefixLength != 64 {
		c.reject(r, "prefixLength", "unsupported_semantics", "native delegated address intent allocates /64; /128 and other installed masks are not interchangeable")
	}
	if s.SendRA || s.Announce {
		c.reject(r, "sendRA", "unsupported_semantics", "legacy implicit RA/dnsmasq constructor behavior requires explicit native router, prefix and DNS lifetime choices")
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
	delegation := serviceconfig.Delegation{Name: c.serviceName(r, "lan"), Interface: name, SubnetID: subnet, HostID: host}
	pd.Delegations = append(pd.Delegations, delegation)
	c.target(r, "services/"+c.document.Services[index].Name+"/delegations/"+delegation.Name)
}
