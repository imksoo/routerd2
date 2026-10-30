// SPDX-License-Identifier: BSD-3-Clause
//
// Static DHCP composition follows routerd/pkg/controller/chain at
// SourceRevision, not the older standalone dnsmasq export renderer. See NOTICE.

package legacyconfig

import (
	"encoding/binary"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// dhcpServers compiles an owned, statically addressed local server. It never
// inspects an installed dnsmasq, a lease database, or the host's interface list.
// The active legacy chain shares one dnsmasq process; disjoint pools and a
// combined capacity <= its default 1000 leases avoid inventing cross-worker
// pool selection, reservation ownership or a larger lease budget.
func (c *compiler) dhcpServers(vrrpNames map[string]string) {
	var servers, reservations []resource
	for _, r := range c.resources {
		switch r.Kind {
		case "DHCPv4Server":
			servers = append(servers, r)
		case "DHCPv4Reservation":
			reservations = append(reservations, r)
		}
	}
	// Every range, selector and reservation originally inhabited one dnsmasq
	// tag namespace. Splitting a colliding namespace would silently repair an
	// ambiguous old configuration and can change which clients receive options.
	tags := map[string]bool{}
	claimTag := func(r resource, field, raw string) {
		tag := strings.NewReplacer("_", "-", ".", "-").Replace(raw)
		valid := tag != ""
		for _, ch := range tag {
			valid = valid && (ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-')
		}
		if !valid || tags[tag] {
			c.reject(r, field, "ambiguous_identity", "legacy DHCP range, selector and reservation tags must be distinct plain identities after dot/underscore normalization")
		}
		tags[tag] = true
	}
	for _, r := range servers {
		claimTag(r, "", r.Metadata.Name)
		for i, scope := range r.Spec.(api.DHCPv4ServerSpec).Scopes {
			field := "scopes[" + strconv.Itoa(i) + "].name"
			claimTag(r, field, r.Metadata.Name+"-"+scope.Name)
			claimTag(r, field, r.Metadata.Name+"-"+scope.Name+"-match")
		}
	}
	for _, r := range reservations {
		claimTag(r, "", r.Metadata.Name)
	}
	attached := map[string][]resource{}
	for _, r := range reservations {
		c.allow(r, "server", "scope", "macAddress", "ipAddress", "hostname", "leaseTime", "options")
		s := r.Spec.(api.DHCPv4ReservationSpec)
		if s.Scope != "" {
			c.reject(r, "scope", "unsupported_semantics", "the pinned validator rejects reservation scope and its active controller skips it; use an unambiguous server reference")
		}
		if s.LeaseTime != "" {
			c.reject(r, "leaseTime", "unsupported_semantics", "the active legacy reservation renderer does not apply this field; an explicit native per-reservation lease is a separate change")
		}
		c.dhcpOptions(r, "options", s.Options)
		var server *resource
		if s.Server == "" {
			if len(servers) == 1 {
				server = &servers[0]
				c.note(r, "The sole DHCPv4Server owns this formerly unqualified reservation.")
			} else {
				c.reject(r, "server", "ambiguous_reference", "a global reservation requires exactly one server or an explicit server name")
			}
		} else if strings.Contains(s.Server, "/") {
			c.reject(r, "server", "reference", "the pinned reservation schema references a bare DHCPv4Server name, not a Kind/name alias")
		} else {
			server = c.getRef(r, "server", s.Server, "DHCPv4Server")
		}
		if server != nil {
			attached[server.ID()] = append(attached[server.ID()], r)
		}
	}
	type compiledServer struct {
		resource resource
		config   dhcpserverconfig.Config
	}
	var compiled []compiledServer
	var capacity uint64
	for _, r := range servers {
		cfg, ok := c.dhcpServer(r, attached[r.ID()], vrrpNames)
		if !ok {
			continue
		}
		for _, previous := range compiled {
			left, _ := netip.ParsePrefix(cfg.Primary.Prefix)
			right, _ := netip.ParsePrefix(previous.config.Primary.Prefix)
			if cfg.Interface == previous.config.Interface || left.Overlaps(right) {
				c.reject(r, "interface", "ambiguous_ownership", "independent native DHCP workers require distinct interfaces and disjoint subnets; the shared legacy server's cross-pool selection cannot be inferred")
			}
		}
		capacity += dhcpCapacity(cfg)
		compiled = append(compiled, compiledServer{r, cfg})
	}
	if capacity > 1000 && len(compiled) > 1 {
		for _, entry := range compiled {
			c.reject(entry.resource, "addressPool", "unsupported_semantics", "combined DHCP capacity exceeds the old shared dnsmasq limit of 1000 leases; splitting that budget requires explicit native capacity intent")
		}
	}
}

func (c *compiler) dhcpServer(r resource, reservations []resource, vrrpNames map[string]string) (dhcpserverconfig.Config, bool) {
	before := len(c.issues)
	c.allow(r, "server", "managed", "role", "listenInterfaces", "logDHCP", "stickyHoldDays", "dns", "interface", "addressPool", "profiles", "scopes", "rangeStart", "rangeEnd", "leaseTime", "routerSource", "router", "gateway", "gatewayFrom", "dnsSource", "dnsInterface", "dnsServers", "dnsServerFrom", "ntpServers", "ntpServerFrom", "domain", "domainFrom", "options", "authoritative", "leaseFile")
	s := r.Spec.(api.DHCPv4ServerSpec)
	for field, active := range map[string]bool{
		"managed": s.Managed, "routerSource": s.RouterSource != "", "router": s.Router != "",
		"dnsSource": s.DNSSource != "", "dnsInterface": s.DNSInterface != "",
		"authoritative": s.Authoritative,
	} {
		if active {
			c.reject(r, field, "unsupported_semantics", "this field is ignored or interpreted differently by the pinned active DHCP chain and standalone renderer; select explicit native intent before migration")
		}
	}
	if s.Server != "" && s.Server != "dnsmasq" {
		c.reject(r, "server", "unsupported_semantics", "external kea/dhcpd backend behavior is not an equivalent native server configuration")
	}
	if s.Role != "" && s.Role != "server" {
		c.reject(r, "role", "unsupported_semantics", "transit/relay behavior is not a local DHCP server")
	}
	if s.LeaseFile != "" {
		c.reject(r, "leaseFile", "unsupported_semantics", "a legacy lease file is not a native journal; importing active or HA-replicated leases is a separate migration")
	}

	if !reflect.DeepEqual(s.DNS, api.DHCPv4ServerDNSSpec{}) {
		c.reject(r, "dns", "unsupported_semantics", "embedded dnsmasq forwarding/cache settings require a separate explicitly compiled DNSResolver")
	}
	ifname := c.dhcpInterface(r, "interface", s.Interface)
	iface := c.ensureInterface(ifname)
	if iface != nil && iface.Master != nil && *iface.Master != "" {
		c.reject(r, "interface", "unsupported_semantics", "DHCP on a bridge port or VRF member requires explicit native binding intent; a master binding is not inferred from old dnsmasq")
	}
	for i, ref := range s.ListenInterfaces {
		if len(s.ListenInterfaces) != 1 || ref != s.Interface {
			c.reject(r, "listenInterfaces["+strconv.Itoa(i)+"]", "unsupported_semantics", "the active local server can migrate only a single listen interface identical to its interface reference")
		}
	}
	s.ListenInterfaces = nil
	source := c.dhcpSubnet(r, "addressPool", ifname, dhcpFirst(s.AddressPool.Start, s.RangeStart), dhcpFirst(s.AddressPool.End, s.RangeEnd))
	for _, other := range c.document.Services {
		if other.DHCP4 != nil && other.DHCP4.Interface == ifname {
			c.reject(r, "interface", "unsupported_semantics", "a DHCP client can change this interface's address; a local server requires an exclusively static IPv4 source")
		}
	}
	if !source.IsValid() {
		return dhcpserverconfig.Config{}, false
	}
	for _, pair := range []struct{ field, pool, alias string }{
		{"rangeStart", s.AddressPool.Start, s.RangeStart}, {"rangeEnd", s.AddressPool.End, s.RangeEnd}, {"leaseTime", s.AddressPool.LeaseTime, s.LeaseTime},
	} {
		if pair.pool != "" && pair.alias != "" && pair.pool != pair.alias {
			c.reject(r, pair.field, "ambiguous_value", "addressPool and legacy range/lease aliases disagree; the shadowed declaration cannot be silently discarded")
		}
	}
	s.AddressPool.Start = dhcpFirst(s.AddressPool.Start, s.RangeStart)
	s.AddressPool.End = dhcpFirst(s.AddressPool.End, s.RangeEnd)
	s.AddressPool.LeaseTime = dhcpFirst(s.AddressPool.LeaseTime, s.LeaseTime, "12h")
	s.RangeStart, s.RangeEnd, s.LeaseTime = "", "", ""
	s.Interface = ifname
	servingWhen := c.when(r, s.When, vrrpNames)
	if !reflect.DeepEqual(s.GatewayFrom, api.StatusValueSourceSpec{}) {
		if s.Gateway != "" {
			c.reject(r, "gatewayFrom", "unsupported_semantics", "literal gateway plus status fallback must be resolved explicitly before migration")
		}
		s.Gateway = c.dhcpAddressSource(r, "gatewayFrom", s.GatewayFrom, servingWhen)
		s.GatewayFrom = api.StatusValueSourceSpec{}
	}
	s.DNSServers = c.dhcpServerSources(r, "dnsServerFrom", s.DNSServers, s.DNSServerFrom, servingWhen)
	s.NTPServers = c.dhcpServerSources(r, "ntpServerFrom", s.NTPServers, s.NTPServerFrom, servingWhen)
	s.DNSServerFrom, s.NTPServerFrom = nil, nil
	if !reflect.DeepEqual(s.DomainFrom, api.StatusValueSourceSpec{}) {
		if s.Domain != "" {
			c.reject(r, "domainFrom", "invalid_value", "domain and domainFrom are mutually exclusive in the pinned schema")
		}
		s.Domain = c.dhcpStaticDomain(r, s.DomainFrom)
		s.DomainFrom = api.StatusValueSourceSpec{}
	}
	// dnsmasq supplies its local address as router and DNS when no override is
	// emitted. Native omission means omit, so materialize those verified defaults.
	if s.Gateway == "" {
		s.Gateway = source.Addr().String()
	}
	if len(s.DNSServers) == 0 {
		s.DNSServers = []string{source.Addr().String()}
	}
	c.dhcpOptions(r, "options", s.Options)
	s.Profiles = append([]api.DHCPv4ServerProfileSpec(nil), s.Profiles...)
	s.Scopes = append([]api.DHCPv4ServerScopeSpec(nil), s.Scopes...)
	usedProfiles := map[string]bool{}
	for _, scope := range s.Scopes {
		usedProfiles[scope.ProfileRef] = true
	}
	for i := range s.Profiles {
		p := &s.Profiles[i]
		path := "profiles[" + strconv.Itoa(i) + "]"
		if !usedProfiles[p.Name] {
			c.reject(r, path, "unreferenced_resource", "a profile not selected by any scope would disappear from native intent")
		}
		c.dhcpOptions(r, path+".options", p.Options)
		p.AddressPool.LeaseTime = dhcpFirst(p.AddressPool.LeaseTime, s.AddressPool.LeaseTime)
		scopeSource := c.dhcpSubnet(r, path+".addressPool", ifname, p.AddressPool.Start, p.AddressPool.End)
		if scopeSource.IsValid() {
			p.Gateway = dhcpFirst(p.Gateway, scopeSource.Addr().String())
			p.Netmask = dhcpFirst(p.Netmask, net.IP(net.CIDRMask(scopeSource.Bits(), 32)).String())
			if len(p.DNSServers) == 0 {
				p.DNSServers = []string{scopeSource.Addr().String()}
			}
		}
	}
	for i := range s.Scopes {
		scope := &s.Scopes[i]
		path := "scopes[" + strconv.Itoa(i) + "]"
		c.dhcpOptions(r, path+".options", scope.Options)
		if scope.ProfileRef == "" {
			scope.AddressPool.LeaseTime = dhcpFirst(scope.AddressPool.LeaseTime, s.AddressPool.LeaseTime)
			scopeSource := c.dhcpSubnet(r, path+".addressPool", ifname, scope.AddressPool.Start, scope.AddressPool.End)
			if scopeSource.IsValid() {
				scope.Gateway = dhcpFirst(scope.Gateway, scopeSource.Addr().String())
				scope.Netmask = dhcpFirst(scope.Netmask, net.IP(net.CIDRMask(scopeSource.Bits(), 32)).String())
				if len(scope.DNSServers) == 0 {
					scope.DNSServers = []string{scopeSource.Addr().String()}
				}
			}
		}
	}
	var inputReservations []api.DHCPv4ReservationSpec
	for _, item := range reservations {
		reservation := item.Spec.(api.DHCPv4ReservationSpec)
		reservation.When = api.ResourceWhenSpec{} // leaseSync validates the original owner condition.
		inputReservations = append(inputReservations, reservation)
	}
	service := serviceconfig.Service{Name: c.serviceName(r, "dhcp"), DHCPServer: &serviceconfig.DHCPServer{Interface: ifname, ServerAddress: source.Addr().String(), Primary: dhcpserverconfig.Scope{Prefix: source.Masked().String()}, MaxLeases: 1000}}
	if len(c.issues) != before {
		return dhcpserverconfig.Config{}, false
	}
	s.When = api.ResourceWhenSpec{} // leaseSync keeps the original condition on the HA owner.
	cfg, err := dhcpserverconfig.FromSpec(service.DHCPServerConfig("/var/lib/routerd"), s, inputReservations)
	if err != nil {
		c.reject(r, "", "native_validation", "DHCP pool, scope, reservation, option or duration cannot be represented by the native server")
		return dhcpserverconfig.Config{}, false
	}
	// Active chain gives the parent domain only to its catch-all range. The
	// general FromSpec adapter also permits intentional native inheritance, which
	// this strict legacy path must not enable accidentally.
	for i := range cfg.Scopes {
		cfg.Scopes[i].Domain = ""
	}
	c.dhcpScopeBoundaries(r, cfg)
	if len(c.issues) != before {
		return dhcpserverconfig.Config{}, false
	}
	for i := range cfg.Scopes {
		cfg.Scopes[i].Options = dhcpBroadcastDefault(cfg.Scopes[i])
	}
	cfg.Primary.Options = dhcpBroadcastDefault(cfg.Primary)
	if err := dhcpserverconfig.Validate(cfg); err != nil {
		c.reject(r, "", "native_validation", "materialized DHCP defaults fail native wire validation")
		return dhcpserverconfig.Config{}, false
	}
	service.DHCPServer.Primary, service.DHCPServer.Scopes, service.DHCPServer.Reservations = cfg.Primary, cfg.Scopes, cfg.Reservations
	service.DHCPServer.StickyHoldDays = cfg.StickyHoldDays
	service.DHCPServer.LogDHCP = cfg.LogDHCP
	service.DHCPServer.FingerprintRetention = c.fingerprintRetention
	c.addService(r, service)
	c.note(r, "Static DHCP intent follows the pinned active chain. One worker supervises the pinned dnsmasq engine and observes its lease file; old lease/sticky/HA state is not imported. The dnsmasq router, DNS and broadcast defaults and 1000-lease limit are explicit.")
	for _, item := range reservations {
		c.target(item, "services/"+service.Name+"/dhcp_server/reservations")
	}
	return cfg, true
}

// dnsmasq selects a local subnet from the address range, not from the number
// of IPv4 addresses on the link. Keep that selection in configured intent.
func (c *compiler) dhcpSubnet(r resource, field, ifname, first, last string) netip.Prefix {
	start, startErr := netip.ParseAddr(first)
	end, endErr := netip.ParseAddr(last)
	if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() {
		c.reject(r, field, "invalid_value", "DHCP range requires IPv4 start and end addresses")
		return netip.Prefix{}
	}
	var source netip.Prefix
	for _, static := range c.static {
		if static.iface != ifname || meaningful(child(static.resource.node, "when")) || !static.prefix.Contains(start) || !static.prefix.Contains(end) {
			continue
		}
		if source.IsValid() && source != static.prefix {
			c.reject(r, "interface", "ambiguous_reference", "multiple owned addresses cover the DHCP pool; select an unambiguous server identity")
			return netip.Prefix{}
		}
		source = static.prefix
	}
	if !source.IsValid() || source.Bits() < 1 || source.Bits() > 30 {
		c.reject(r, "interface", "ambiguous_reference", "DHCP pool requires an unconditional owned IPv4 address on its interface with a /1../30 prefix")
		return netip.Prefix{}
	}
	return source
}

func (c *compiler) dhcpAddressSource(r resource, path string, source api.StatusValueSourceSpec, servingWhen *resourceprogram.FactCondition) string {
	if source.Resource == "" || source.Field == "" {
		c.reject(r, path, "required", "a DHCP address source must explicitly name a resource and address field")
		return ""
	}
	if ref := c.byRef[source.Resource]; ref != nil && ref.Kind == "VirtualAddress" {
		vip := ref.Spec.(api.VirtualAddressSpec)
		if vip.Mode == "vrrp" && source.Field == "address" && !source.Optional {
			owner := c.leaseService(*ref, "vrrp")
			if owner == nil || owner.VRRP.Family != "ipv4" || !reflect.DeepEqual(servingWhen, &resourceprogram.FactCondition{Fact: owner.ModuleName() + ".elected"}) {
				c.reject(r, path, "unsupported_semantics", "the DHCP server must serve under the referenced IPv4 virtual address's master condition")
				return ""
			}
			// The address is fixed intent; only its publication changes. The
			// same HA owner gates DHCP serving after installing this address.
			address, _ := netip.ParsePrefix(owner.VRRP.Addresses[0])
			return address.Addr().String()
		}
	}
	return c.resolveStaticAddress(r, path, source, true)
}

func (c *compiler) dhcpInterface(r resource, field, ref string) string {
	if ref == "" || strings.Contains(ref, "/") {
		c.reject(r, field, "reference", "the active DHCP chain requires a bare Interface or Bridge resource name, not a qualified or literal kernel alias")
		return ""
	}
	if c.byRef["Interface/"+ref] != nil {
		return c.interfaceName(r, field, ref, false)
	}
	if bridge := c.byRef["Bridge/"+ref]; bridge != nil {
		spec := bridge.Spec.(api.BridgeSpec)
		if spec.IfName != "" && spec.IfName != bridge.Metadata.Name {
			c.reject(r, field, "unsupported_semantics", "the active DHCP chain uses a bridge's resource name as its kernel name; a different ifname is not an equivalent binding")
		}
		return c.interfaceName(r, field, ref, false)
	}
	c.reject(r, field, "unsupported_semantics", "the active DHCP chain does not resolve this alias as a static local Ethernet Interface or Bridge")
	return ""
}

func (c *compiler) dhcpServerSources(r resource, path string, literal []string, sources []api.StatusValueSourceSpec, servingWhen *resourceprogram.FactCondition) []string {
	result := append([]string(nil), literal...)
	for i, source := range sources {
		if address := c.dhcpAddressSource(r, path+"["+strconv.Itoa(i)+"]", source, servingWhen); address != "" {
			result = append(result, address)
		}
	}
	return result
}

func (c *compiler) dhcpStaticDomain(r resource, source api.StatusValueSourceSpec) string {
	if source.Optional || source.Field != "zone" || !strings.HasPrefix(source.Resource, "DNSZone/") {
		c.reject(r, "domainFrom", "unsupported_semantics", "domainFrom must be a required static DNSZone/name reference with field zone")
		return ""
	}
	zone := c.getRef(r, "domainFrom", source.Resource, "DNSZone")
	if zone == nil {
		return ""
	}
	if meaningful(child(zone.node, "when")) {
		c.reject(r, "domainFrom", "unsupported_semantics", "a conditional DNS zone cannot supply a static DHCP domain")
		return ""
	}
	return zone.Spec.(api.DNSZoneSpec).Zone
}

// Options retain routerd's dnsmasq syntax. The engine parses them; importing
// intent must not impose an unrelated native wire encoder's restrictions.
func (c *compiler) dhcpOptions(r resource, path string, options []api.DHCPv4OptionSpec) {
	if err := dhcpserverconfig.ValidateOptions(options); err != nil {
		c.reject(r, path, "invalid_value", err.Error())
	}
}

func (c *compiler) dhcpScopeBoundaries(r resource, cfg dhcpserverconfig.Config) {
	scopes := append([]dhcpserverconfig.Scope{cfg.Primary}, cfg.Scopes...)
	for i, scope := range scopes {
		path := "addressPool"
		if i != 0 {
			path = "scopes[" + strconv.Itoa(i-1) + "]"
		}
		source := c.dhcpSubnet(r, path, cfg.Interface, scope.Start, scope.End)
		if source.IsValid() && scope.Prefix != source.Masked().String() {
			c.reject(r, path, "unsupported_semantics", "DHCP netmask must match the owned interface subnet containing the pool")
		}
		start, _ := netip.ParseAddr(scope.Start)
		end, _ := netip.ParseAddr(scope.End)
		for j, earlier := range scopes[:i] {
			a, _ := netip.ParseAddr(earlier.Start)
			b, _ := netip.ParseAddr(earlier.End)
			if start.Compare(b) <= 0 && a.Compare(end) <= 0 {
				// Different device selectors may share the same profile/range in
				// dnsmasq. No allocation priority is inferred for identical intent.
				left, right := earlier, scope
				left.Name, right.Name = "", ""
				left.Match, right.Match = dhcpserverconfig.Match{}, dhcpserverconfig.Match{}
				if j == 0 || !reflect.DeepEqual(left, right) {
					c.reject(r, path, "ambiguous_value", "overlapping DHCP pools with different intent cannot be assigned an inferred allocation priority")
				}
			}
			if i > 0 && dhcpSelectorsOverlap(earlier.Match, scope.Match) {
				c.reject(r, path+".match", "ambiguous_value", "a client matching different legacy scopes has no portable allocation priority; make selectors disjoint")
			}
		}
	}
}

func dhcpSelectorsOverlap(a, b dhcpserverconfig.Match) bool {
	for _, raw := range a.MACAddresses {
		mac, _ := net.ParseMAC(raw)
		for _, other := range b.MACAddresses {
			v, _ := net.ParseMAC(other)
			if mac.String() == v.String() {
				return true
			}
		}
		for _, oui := range b.OUIPrefixes {
			if strings.HasPrefix(mac.String(), strings.ToLower(oui)+":") {
				return true
			}
		}
	}
	for _, oui := range a.OUIPrefixes {
		for _, raw := range b.MACAddresses {
			mac, _ := net.ParseMAC(raw)
			if strings.HasPrefix(mac.String(), strings.ToLower(oui)+":") {
				return true
			}
		}
		for _, other := range b.OUIPrefixes {
			if strings.EqualFold(oui, other) {
				return true
			}
		}
	}
	return false
}

func dhcpBroadcastDefault(scope dhcpserverconfig.Scope) []dhcpserverconfig.Option {
	options := append([]dhcpserverconfig.Option(nil), scope.Options...)
	for _, option := range options {
		if option.Code == 28 || strings.EqualFold(option.Name, "broadcast") {
			return options
		}
	}
	prefix, _ := netip.ParsePrefix(scope.Prefix)
	address := prefix.Addr().As4()
	broadcast := binary.BigEndian.Uint32(address[:]) | ^uint32(0)>>prefix.Bits()
	binary.BigEndian.PutUint32(address[:], broadcast)
	return append(options, dhcpserverconfig.Option{Code: 28, Value: netip.AddrFrom4(address).String()})
}

func dhcpCapacity(cfg dhcpserverconfig.Config) uint64 {
	var count uint64
	scopes := append([]dhcpserverconfig.Scope{cfg.Primary}, cfg.Scopes...)
	for _, scope := range scopes {
		start, _ := netip.ParseAddr(scope.Start)
		end, _ := netip.ParseAddr(scope.End)
		a, b := start.As4(), end.As4()
		count += uint64(binary.BigEndian.Uint32(b[:])) - uint64(binary.BigEndian.Uint32(a[:])) + 1
	}
	for _, reservation := range cfg.Reservations {
		address, _ := netip.ParseAddr(reservation.IPAddress)
		inside := false
		for _, scope := range scopes {
			start, _ := netip.ParseAddr(scope.Start)
			end, _ := netip.ParseAddr(scope.End)
			inside = inside || start.Compare(address) <= 0 && address.Compare(end) <= 0
		}
		if !inside {
			count++
		}
	}
	return count
}

func dhcpFirst(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
