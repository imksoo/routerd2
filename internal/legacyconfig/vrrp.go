// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net"
	"net/netip"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) vrrp(r resource, s api.VirtualAddressSpec) (string, []resourceprogram.Resource) {
	policy, ok := c.options.VRRP[r.Kind+"/"+r.Metadata.Name]
	if !ok {
		c.reject(r, "mode", "unsupported_semantics", "VRRP requires explicit vrrp policy selecting the native election source and VIP VMAC interface")
		return "", nil
	}
	c.usePolicy(r, "vrrp")
	if policy.SourceAddress == "" || policy.VIPInterface == "" {
		c.policyIssue(r, "vrrp", "", "policy_required", "source_address and vip_interface are required; the native VMAC replaces physical-interface VIP publication")
	}
	if s.Hostname != "" || s.ExternalDNS {
		c.reject(r, "hostname", "unsupported_semantics", "automatic VIP hostname publication requires explicit native DNS intent")
	}
	if meaningful(child(r.node, "addressFrom")) {
		c.reject(r, "addressFrom", "unsupported_semantics", "dynamic VIP sources require their complete native producer binding")
	}
	if len(s.Track) != 0 {
		c.reject(r, "track", "unsupported_semantics", "tracking needs its complete native source graph; it cannot be dropped")
	}
	if s.VRRP.AuthenticationFrom.File != "" || s.VRRP.AuthenticationFrom.Env != "" || s.VRRP.AuthenticationFrom.Base64 {
		c.reject(r, "vrrp.authenticationFrom", "unsupported_semantics", "secret references are not read or converted to inline authentication by migration")
	}
	address, err := netip.ParsePrefix(s.Address)
	if err != nil {
		if a, parseErr := netip.ParseAddr(s.Address); parseErr == nil {
			address, err = netip.PrefixFrom(a, a.BitLen()), nil
		}
	}
	if err != nil || address.Addr().Is4In6() || s.Family != "ipv4" && s.Family != "ipv6" || address.Addr().Is4() != (s.Family == "ipv4") {
		c.reject(r, "address", "invalid_value", "VRRP requires a literal address or address/prefix of its declared family")
		return "", nil
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	c.ensureInterface(name)
	service := c.serviceName(r, "vrrp")
	preempt := false // The original Keepalived renderer emits nopreempt by default.
	if s.VRRP.Preempt != nil {
		preempt = *s.VRRP.Preempt
	}
	cfg := vrrp.Config{Version: 2, Interface: name, VIPInterface: policy.VIPInterface,
		Family: s.Family, SourceAddress: policy.SourceAddress, VirtualRouterID: s.VRRP.VirtualRouterID,
		Priority: s.VRRP.Priority, Preempt: &preempt, AdvertIntervalCentiseconds: 100,
		Addresses: []string{address.String()}, AcceptMode: true, AuthPassword: s.VRRP.Authentication}
	if s.Family == "ipv6" {
		cfg.Version = 3
		cfg.Addresses = append([]string{policy.VIPLinkLocalAddress}, cfg.Addresses...)
	} else if policy.VIPLinkLocalAddress != "" {
		c.policyIssue(r, "vrrp", "vip_link_local_address", "conflict", "a VIP link-local address belongs to an IPv6 election")
	}
	for _, value := range s.VRRP.Peers {
		peer, err := netip.ParseAddr(value)
		if err != nil {
			c.reject(r, "vrrp.peers", "invalid_value", "peer must be a same-family unicast IP literal")
			continue
		}
		cfg.UnicastPeers = append(cfg.UnicastPeers, peer)
	}
	c.addService(r, serviceconfig.Service{Name: service, VRRP: &cfg})
	c.note(r, "Preserves the original Keepalived IPv4 v2/IPv6 v3 wire version, one-second advertisements, priority and peers. Omitted preempt remains false. Explicit local identities replace physical-interface VIPs with native VMAC ownership and GARP/NA; the original zone's ACL also covers that VMAC. Additional VMACs become elected links with inherited address/consumer dependencies, normal DAD and original finite authority. Dependent RA teardown owns withdrawal; no second RA sender is added. Unsupported readiness/tracking declarations and runtime conditions are not erased.")
	var nodes []resourceprogram.Resource
	for _, v := range failoverVMACs(s.VRRP) {
		parent := c.interfaceName(r, "vrrp.failoverVMAC.parentInterface", v.ParentInterface, false)
		c.ensureInterface(parent)
		c.ensureInterface(v.Interface)
		mac, err := net.ParseMAC(v.MACAddress)
		if err != nil {
			c.reject(r, "vrrp.failoverVMAC.macAddress", "invalid_value", "VMAC requires an Ethernet MAC address")
			continue
		}
		link := network.Link{Name: v.Interface, Kind: "macvlan", Parent: parent, MACVLANMode: "bridge", HardwareAddr: mac.String()}
		if v.LinkLocalAddress != "" {
			a, err := netip.ParseAddr(v.LinkLocalAddress)
			if err != nil || !a.Is6() || !a.IsLinkLocalUnicast() {
				c.reject(r, "vrrp.failoverVMAC.linkLocalAddress", "invalid_value", "VMAC link-local must be an IPv6 link-local address")
				continue
			}
			link.IPv6AddrGenMode = "none"
			nodes = append(nodes, resourceprogram.Resource{ID: "vmac-address/" + v.Interface, Address: &resourceprogram.Address{Interface: v.Interface, CIDR: netip.PrefixFrom(a, 64).String()}})
		}
		nodes = append(nodes, resourceprogram.Resource{ID: "link/" + v.Interface, Link: &link, When: &resourceprogram.FactCondition{Fact: "service." + service + ".elected"}})
	}
	return service, nodes
}

func failoverVMACs(s api.VirtualAddressVRRPSpec) []api.VirtualAddressVRRPFailoverVMACSpec {
	if s.FailoverVMAC == nil {
		return s.AdditionalFailoverVMACs
	}
	return append([]api.VirtualAddressVRRPFailoverVMACSpec{*s.FailoverVMAC}, s.AdditionalFailoverVMACs...)
}
