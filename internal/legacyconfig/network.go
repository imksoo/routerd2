// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
)

func (c *compiler) collectAliases() {
	for _, r := range c.resources {
		name, managed := "", false
		switch s := r.Spec.(type) {
		case api.InterfaceSpec:
			name = s.IfName
			managed = s.Managed && s.Owner != "external" && s.AdminUp
		case api.LinkSpec:
			name = first(s.IfName, r.Metadata.Name)
		case api.VRFSpec:
			name, managed = first(s.IfName, r.Metadata.Name), true
		case api.BridgeSpec:
			name, managed = first(s.IfName, r.Metadata.Name), true
		case api.VXLANTunnelSpec:
			name, managed = first(s.IfName, r.Metadata.Name), true
		case api.VXLANSegmentSpec:
			name, managed = first(s.IfName, r.Metadata.Name), true
		case api.DSLiteTunnelSpec:
			name, managed = first(s.TunnelName, s.Interface, r.Metadata.Name), true
		default:
			continue
		}
		if name == "" {
			c.reject(r, "ifname", "required", "kernel interface name is required")
			continue
		}
		if previous, exists := c.aliasOwner[r.Metadata.Name]; exists && previous != r.ID() {
			c.reject(r, "", "alias_collision", "interface resource names must be unambiguous across kinds")
			continue
		}
		c.aliasOwner[r.Metadata.Name] = r.ID()
		c.aliases[r.Metadata.Name] = name
		c.aliases[r.Kind+"/"+r.Metadata.Name] = name
		if managed {
			c.upAllowed[name] = true
		}
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (c *compiler) interfaceName(r resource, field, value string, literal bool) string {
	if name, ok := c.aliases[value]; ok {
		return name
	}
	if literal && value != "" && !strings.Contains(value, "/") {
		return value
	}
	c.reject(r, field, "reference", "interface must resolve to a declared Interface, Link, or supported virtual link")
	return ""
}

func (c *compiler) ensureInterface(r resource, field, name string) *network.Interface {
	if name == "" {
		return nil
	}
	if !c.upAllowed[name] {
		c.reject(r, field, "administrative_state", "native address/membership management brings the link up; declare a managed adminUp interface to authorize that behavior")
		return nil
	}
	if c.interfaces[name] == nil {
		c.interfaces[name] = &network.Interface{Name: name}
	}
	return c.interfaces[name]
}

func (c *compiler) iface(r resource) {
	c.allow(r, "ifname", "adminUp", "managed", "owner", "mtu")
	s := r.Spec.(api.InterfaceSpec)
	if s.Owner != "" && s.Owner != "routerd" && s.Owner != "external" {
		c.reject(r, "owner", "invalid_value", "owner must be routerd or external")
	}
	if !s.Managed || s.Owner == "external" || !s.AdminUp {
		if s.MTU != 0 {
			c.reject(r, "mtu", "administrative_state", "an unmanaged or non-adminUp link cannot acquire native MTU ownership implicitly")
		}
		c.note(r, "Existing interface is observed by name; administrative state is left unmanaged.")
		return
	}
	if iface := c.ensureInterface(r, "ifname", s.IfName); iface != nil {
		if iface.MTU != 0 && s.MTU != 0 && iface.MTU != s.MTU {
			c.reject(r, "mtu", "conflict", "conflicting MTU declarations for the same kernel interface")
		}
		if s.MTU != 0 {
			iface.MTU = s.MTU
		}
		c.target(r, "network.interfaces/"+s.IfName)
	}
}

func (c *compiler) member(r resource, member, master string) {
	name := c.interfaceName(r, "members", member, false)
	if iface := c.ensureInterface(r, "members", name); iface != nil {
		if iface.Master != nil && *iface.Master != master {
			c.reject(r, "members", "conflict", "interface is assigned to more than one master")
			return
		}
		iface.Master = &master
		c.target(r, "network.interfaces/"+name+"/master")
	}
}

func (c *compiler) vrf(r resource) {
	c.allow(r, "ifname", "routeTable", "members")
	s := r.Spec.(api.VRFSpec)
	name := c.aliases[r.Kind+"/"+r.Metadata.Name]
	c.document.Network.Links = append(c.document.Network.Links, network.Link{Name: name, Kind: "vrf", VRFTable: s.RouteTable})
	c.ensureInterface(r, "ifname", name)
	c.target(r, "network.links/"+name)
	for _, m := range s.Members {
		c.member(r, m, name)
	}
}

func (c *compiler) bridge(r resource) {
	c.allow(r, "ifname", "members", "stp", "rstp", "forwardDelay", "helloTime", "macAddress", "mtu", "multicastSnooping")
	s := r.Spec.(api.BridgeSpec)
	if s.STP == nil || *s.STP {
		c.reject(r, "stp", "unsupported_semantics", "legacy STP defaults to enabled; native bridge STP is unavailable, so stp:false is required")
	}
	if s.RSTP == nil || *s.RSTP {
		c.reject(r, "rstp", "unsupported_semantics", "RSTP has no native bridge implementation; rstp:false is required")
	}
	if s.ForwardDelay != 0 || s.HelloTime != 0 {
		c.reject(r, "forwardDelay", "unsupported_semantics", "custom bridge timers are not represented by native links")
	}
	snooping := false
	if s.MulticastSnooping != nil {
		snooping = *s.MulticastSnooping
	}
	name := c.aliases[r.Kind+"/"+r.Metadata.Name]
	c.document.Network.Links = append(c.document.Network.Links, network.Link{Name: name, Kind: "bridge", HardwareAddr: s.MACAddress, BridgeMulticastSnooping: &snooping})
	if iface := c.ensureInterface(r, "ifname", name); iface != nil {
		iface.MTU = s.MTU
	}
	for _, m := range s.Members {
		c.member(r, m, name)
	}
	c.target(r, "network.links/"+name)
	c.note(r, "STP/RSTP must be disabled; dormant bridge timing attributes use kernel defaults.")
}

func (c *compiler) static4(r resource) {
	c.allow(r, "interface", "address", "exclusive", "allowOverlap", "allowOverlapReason")
	s := r.Spec.(api.IPv4StaticAddressSpec)
	if s.Exclusive {
		c.reject(r, "exclusive", "unsupported_semantics", "native ownership never removes addresses owned by another manager")
	}
	if s.AllowOverlapReason != "" && !s.AllowOverlap {
		c.reject(r, "allowOverlapReason", "invalid_value", "overlap justification requires allowOverlap:true")
	}
	if s.AllowOverlap && strings.TrimSpace(s.AllowOverlapReason) == "" {
		c.reject(r, "allowOverlapReason", "required", "the original overlap policy requires a justification")
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	prefix, err := netip.ParsePrefix(s.Address)
	if err != nil || !prefix.Addr().Is4() {
		c.reject(r, "address", "invalid_value", "expected an IPv4 host/prefix address")
		return
	}
	c.static = append(c.static, staticPrefix{resource: r, iface: name, prefix: prefix, allow: s.AllowOverlap})
	if iface := c.ensureInterface(r, "interface", name); iface != nil {
		iface.Addresses = append(iface.Addresses, prefix.String())
		c.target(r, "network.interfaces/"+name+"/addresses")
	}
}

func (c *compiler) virtualAddress(r resource) {
	c.allow(r, "family", "interface", "address", "hostname", "externalDNS", "mode", "vrrp", "track", "addressFrom", "gratuitousARP")
	s := r.Spec.(api.VirtualAddressSpec)
	if s.Mode == "vrrp" {
		c.reject(r, "mode", "unsupported_semantics", "legacy Keepalived defaults, physical-interface VIPs and non-preempt default require explicit peer-aware migration to native VRRP/VMAC with a selected wire version")
		return
	}
	if s.Mode != "" && s.Mode != "static" {
		c.reject(r, "mode", "invalid_value", "expected static or vrrp")
	}
	if meaningful(child(r.node, "vrrp")) || len(s.Track) != 0 {
		c.reject(r, "vrrp", "unsupported_semantics", "VRRP election/tracking settings cannot be applied to a static address")
	}
	if s.Hostname != "" || s.ExternalDNS {
		c.reject(r, "hostname", "unsupported_semantics", "automatic address-to-zone hostname publication is not yet a migration rule; declare the DNS record explicitly")
	}
	if s.GratuitousARP {
		c.reject(r, "gratuitousARP", "unsupported_semantics", "native static address management does not send the legacy arping announcement")
	}
	ipv4 := s.Family == "ipv4"
	if !ipv4 && s.Family != "ipv6" {
		c.reject(r, "family", "invalid_value", "family must be ipv4 or ipv6")
		return
	}
	addr := s.Address
	if meaningful(child(r.node, "addressFrom")) && s.AddressFrom.Resource == "" {
		c.reject(r, "addressFrom.resource", "required", "address source resource is required")
	}
	if s.AddressFrom.Resource != "" {
		if addr != "" {
			c.reject(r, "addressFrom", "unsupported_semantics", "a static literal and a status source cannot be combined during migration")
		}
		addr = c.resolveStaticAddress(r, "addressFrom", s.AddressFrom, ipv4)
		if addr != "" {
			if ipv4 {
				addr += "/32"
			} else {
				addr += "/128"
			}
		}
	}
	prefix, err := netip.ParsePrefix(addr)
	bits := 128
	if ipv4 {
		bits = 32
	}
	if err != nil || prefix.Addr().Is4() != ipv4 || prefix.Addr().Is4In6() || prefix.Bits() != bits {
		c.reject(r, "address", "invalid_value", "VirtualAddress must be one host prefix of its declared address family")
		return
	}
	name := c.interfaceName(r, "interface", s.Interface, false)
	if iface := c.ensureInterface(r, "interface", name); iface != nil {
		iface.Addresses = append(iface.Addresses, prefix.String())
		c.target(r, "network.interfaces/"+name+"/addresses")
	}
}

func (c *compiler) route(r resource, destination, gateway, dev string, metric, family int, literal bool) {
	if destination == "default" {
		if family == 4 {
			destination = "0.0.0.0/0"
		} else {
			destination = "::/0"
		}
	}
	prefix, err := netip.ParsePrefix(destination)
	if err != nil || prefix.Addr().Is4() != (family == 4) || prefix.Addr().Is4In6() {
		c.reject(r, "destination", "invalid_value", "route destination must be a prefix of the declared family")
		return
	}
	if prefix != prefix.Masked() {
		c.reject(r, "destination", "invalid_value", "route destination must be a canonical network prefix")
	}
	if gateway != "" {
		a, err := netip.ParseAddr(gateway)
		if err != nil || a.Is4() != (family == 4) || a.Is4In6() {
			c.reject(r, "gateway", "invalid_value", "gateway must be an address of the route family")
			return
		}
		gateway = a.String()
	}
	name := c.interfaceName(r, "interface", dev, literal)
	if index, ok := c.dynamicTunnels[name]; ok {
		c.dynamicDSLiteRoute(r, index, prefix, gateway, metric, family)
		return
	}
	c.document.Network.Routes = append(c.document.Network.Routes, network.Route{Destination: prefix.String(), Gateway: gateway, Interface: name, Metric: metric})
	c.target(r, "network.routes")
}

func (c *compiler) ipv4Route(r resource) {
	c.allow(r, "destination", "type", "device", "deviceFrom", "gateway", "gatewayFrom", "preferredSource", "metric")
	s := r.Spec.(api.IPv4RouteSpec)
	if s.Type != "" && s.Type != "unicast" {
		c.reject(r, "type", "unsupported_semantics", "native routes currently implement unicast only")
	}
	if meaningful(child(r.node, "deviceFrom")) {
		if s.Device != "" {
			c.reject(r, "deviceFrom", "unsupported_semantics", "a literal route device and a status device cannot be combined during migration")
		}
		ref := c.byRef[s.DeviceFrom.Resource]
		if ref == nil || s.DeviceFrom.Optional || s.DeviceFrom.Field != "device" && s.DeviceFrom.Field != "ifname" && s.DeviceFrom.Field != "interface" && s.DeviceFrom.Field != "tunnelName" {
			c.reject(r, "deviceFrom", "unsupported_semantics", "only an explicit, unconditional static device reference can be resolved during migration")
		} else if s.DeviceFrom.Field == "ifname" && (ref.Kind == "Interface" || ref.Kind == "VRF" || ref.Kind == "Bridge") ||
			ref.Kind == "DSLiteTunnel" && s.DeviceFrom.Field != "ifname" {
			s.Device = c.aliases[ref.Kind+"/"+ref.Metadata.Name]
		} else {
			c.reject(r, "deviceFrom", "unsupported_semantics", "this resource does not publish a statically known value for the selected device field")
		}
	}
	if meaningful(child(r.node, "gatewayFrom")) {
		if s.Gateway != "" {
			c.reject(r, "gatewayFrom", "unsupported_semantics", "a literal gateway and a status gateway cannot be combined during migration")
		}
		s.Gateway = c.resolveStaticAddress(r, "gatewayFrom", s.GatewayFrom, true)
	}
	if s.PreferredSource != "" {
		c.reject(r, "preferredSource", "unsupported_semantics", "native routes do not yet set RTA_PREFSRC")
	}
	c.route(r, s.Destination, s.Gateway, s.Device, s.Metric, 4, true)
}

func (c *compiler) sysctl(r resource) {
	c.allow(r, "key", "value", "expectedValue", "compare", "runtime", "persistent", "optional")
	s := r.Spec.(api.SysctlSpec)
	if s.Persistent || s.Optional || s.Runtime != nil && !*s.Runtime {
		c.reject(r, "", "unsupported_semantics", "only required runtime forwarding sysctls are represented; persistent host files and optional application are not")
	}
	if s.Compare != "" && s.Compare != "exact" || s.ExpectedValue != "" && s.ExpectedValue != s.Value {
		c.reject(r, "compare", "unsupported_semantics", "native forwarding does not implement non-exact observed-value policies")
	}
	if s.Value != "0" && s.Value != "1" {
		c.reject(r, "value", "invalid_value", "forwarding value must be the string 0 or 1")
		return
	}
	value := s.Value == "1"
	var target **bool
	switch s.Key {
	case "net.ipv4.ip_forward":
		target = &c.document.Network.Forwarding.IPv4
	case "net.ipv6.conf.all.forwarding":
		target = &c.document.Network.Forwarding.IPv6
	default:
		c.reject(r, "key", "unsupported_semantics", "only net.ipv4.ip_forward and net.ipv6.conf.all.forwarding have native ownership")
		return
	}
	if *target != nil {
		c.reject(r, "key", "duplicate_sysctl", "each forwarding setting must have one owner")
		return
	}
	*target = &value
	c.target(r, "network.forwarding")
}

func (c *compiler) dslite(r resource) {
	if policy, ok := c.options.DSLite[r.Kind+"/"+r.Metadata.Name]; ok {
		c.dynamicDSLite(r, policy)
		return
	}
	c.staticDSLite(r)
}

func (c *compiler) staticDSLite(r resource) {
	c.allow(r, "interface", "tunnelName", "enabled", "aftrFQDN", "aftrIPv6", "aftrDNSServers", "aftrAddressOrdinal", "aftrAddressSelection", "remoteAddress", "localAddress", "localAddressFrom", "localIPv6Source", "aftrFrom", "aftrSource", "localAddressSource", "localDelegatedAddress", "localAddressSuffix", "defaultRoute", "routeMetric", "mtu", "encapsulationLimit")
	s := r.Spec.(api.DSLiteTunnelSpec)
	if s.Enabled != nil && !*s.Enabled {
		c.reject(r, "enabled", "unsupported_semantics", "disabled legacy tunnels are not equivalent to removing native owned tunnel intent")
	}
	if s.AFTRFQDN != "" || len(s.AFTRDNSServers) > 0 || s.AFTRAddressOrdinal != 0 || s.AFTRAddressSelection != "" || meaningful(child(r.node, "aftrFrom")) || s.AFTRSource != "" {
		c.reject(r, "aftrFQDN", "unsupported_semantics", "legacy local-DNS, source-selection and indefinite pending-tunnel behavior requires explicit resource-scoped ds_lite migration policy; native bootstrap and expiry are not inferred")
	}
	if s.LocalAddressSource != "" && s.LocalAddressSource != "static" && s.LocalAddressSource != "interface" || s.LocalDelegatedAddress != "" || s.LocalIPv6Source != "" || s.LocalAddressSuffix != "" {
		c.reject(r, "localAddressSource", "unsupported_semantics", "only a literal outer IPv6 endpoint is supported; delegated, observed and suffix-derived sources are not frozen")
	}
	if s.LocalAddress == "" && (s.LocalAddressSource == "" || s.LocalAddressSource == "interface") {
		c.reject(r, "localAddressSource", "unsupported_semantics", "kernel-observed outer source selection requires explicit ds_lite policy naming an owned native RA source; external addresses are not adopted")
	}
	if s.EncapsulationLimit != "" && s.EncapsulationLimit != "none" {
		c.reject(r, "encapsulationLimit", "unsupported_semantics", "native DS-Lite suppresses the IPv6 encapsulation-limit option")
	}
	if s.DefaultRoute || s.RouteMetric != 0 {
		c.reject(r, "defaultRoute", "unsupported_semantics", "the pinned Linux tunnel controller does not implement defaultRoute/routeMetric; declare a separate IPv4Route to avoid inventing a route during migration")
	}
	remote := first(s.AFTRIPv6, s.RemoteAddress)
	if s.AFTRIPv6 != "" && s.RemoteAddress != "" && s.AFTRIPv6 != s.RemoteAddress {
		c.reject(r, "remoteAddress", "unsupported_semantics", "multiple differing remote candidates are not represented by a fixed native endpoint")
	}
	local := s.LocalAddress
	for field, value := range map[string]string{"localAddress": local, "aftrIPv6": remote} {
		a, err := netip.ParseAddr(value)
		if err != nil || !a.Is6() || a.Is4In6() || !a.IsGlobalUnicast() || a.IsLoopback() || a.IsLinkLocalUnicast() {
			c.reject(r, field, "invalid_value", "fixed DS-Lite endpoints must be global or unique-local IPv6 unicast addresses")
		}
	}
	inner := "192.0.0.2"
	if meaningful(child(r.node, "localAddressFrom")) {
		// Despite its name, this source selects the inner IPv4 endpoint in
		// the pinned Linux controller; it is not the outer IPv6 source.
		inner = c.dsliteInnerAddress(r, s.LocalAddressFrom)
	}
	innerIP, err := netip.ParseAddr(inner)
	if err != nil || !innerIP.Is4() || innerIP.IsUnspecified() || innerIP.IsMulticast() || innerIP.IsLoopback() {
		c.reject(r, "localAddressFrom", "invalid_value", "inner endpoint must resolve to usable IPv4 unicast")
		return
	}
	underlay := c.interfaceName(r, "interface", s.Interface, false)
	name := c.aliases[r.Kind+"/"+r.Metadata.Name]
	mtu := s.MTU
	if mtu == 0 {
		mtu = 1460 // The old Linux controller default is not underlay-dependent.
	}
	if s.MTU != 0 && (s.MTU < 1280 || s.MTU > 65495) {
		c.reject(r, "mtu", "invalid_value", "MTU must satisfy both the pinned DSLiteTunnel range and the native IPv6 encapsulation limit")
	}
	c.document.Network.Links = append(c.document.Network.Links, network.Link{Name: name, Kind: "ip6tnl", Parent: underlay, Local: local, Remote: remote})
	if iface := c.ensureInterface(r, "tunnelName", name); iface != nil {
		iface.MTU = mtu
		address := innerIP.String() + "/32"
		iface.Addresses = append(iface.Addresses, address)
		if iface.AddressOptions == nil {
			iface.AddressOptions = map[string]network.AddressOptions{}
		}
		iface.AddressOptions[address] = network.AddressOptions{Peer: "192.0.0.1/32"}
	}
	c.target(r, "network.links/"+name)
	c.target(r, "network.interfaces/"+name+"/addresses")
	c.note(r, "Preserves the legacy inner IPv4 peer and fixed 1460-byte default MTU; routes and NAT remain separately declared intent.")
}
