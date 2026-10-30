// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"reflect"
	"sort"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/retention"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Mapping makes structural changes reviewable. Native service identities are
// derived deterministically from kind/name rather than carrying arbitrary legacy
// names into private socket and journal paths.
type Mapping struct {
	Resource string   `json:"resource"`
	Targets  []string `json:"targets"`
	Notes    []string `json:"notes,omitempty"`
}

type Result struct {
	Document       config.Document `json:"document"`
	SourceRouter   string          `json:"source_router"`
	SourceRevision string          `json:"source_revision"`
	Mappings       []Mapping       `json:"mappings"`
}

// Migrate returns a complete, validated native Document or a zero Document and
// *UnsupportedError. It performs no reads, writes, network access, or subprocesses.
func Migrate(data []byte) (config.Document, error) {
	result, err := Compile(data)
	return result.Document, err
}

// Compile additionally returns the source-to-native identity mapping. On any
// issue, the entire Result is zero; callers cannot accidentally apply a subset.
func Compile(data []byte) (Result, error) {
	return compile(data, Options{}, false)
}

// CompileWithOptions accepts explicit, resource-scoped replacement policies for
// legacy behavior that depended on the host OS. Those replacements are recorded
// in Mapping.Notes; they are not evidence of automatic semantic compatibility.
// A policy may not suppress a diagnostic or drop any legacy resource or field.
func CompileWithOptions(data []byte, options Options) (Result, error) {
	return compile(data, options, true)
}

func compile(data []byte, options Options, explicit bool) (Result, error) {
	raw, resources, d := decode(data)
	c := &compiler{
		decoder: d, resources: resources,
		document: config.Native{},
		byRef:    map[string]*resource{}, aliases: map[string]string{},
		aliasOwner: map[string]string{}, upAllowed: map[string]bool{},
		interfaces: map[string]*network.Interface{}, names: map[string]string{},
		pd: map[string]int{}, raClients: map[string]int{}, information: map[string]int{},
		dynamicTunnels: map[string]int{}, dynamicRoutes: map[string]bool{},
		mappings: map[string]*Mapping{},
	}
	for i := range resources {
		c.legacy.Spec.Resources = append(c.legacy.Spec.Resources, resources[i].Resource)
		r := &resources[i]
		ref := r.Kind + "/" + r.Metadata.Name
		if c.byRef[ref] != nil {
			c.issue(r.node, r.path+".metadata.name", "ambiguous_reference", "kind/name must identify exactly one legacy resource across all API versions")
		} else {
			c.byRef[ref] = r
		}
		c.mappings[r.ID()] = &Mapping{Resource: r.Kind + "/" + r.Metadata.Name}
	}
	c.initOptions(options, explicit)
	c.collectAliases()
	c.managementAccess()
	var extra []resourceprogram.Resource
	var conditions []resourceCondition
	var informationConditions []resourceCondition
	vrrpNames := map[string]string{}
	c.wireguard()
	for _, r := range resources {
		switch r.Kind {
		case "Interface":
			c.iface(r)
		case "Link":
			c.allow(r, "ifname")
			c.note(r, "External link name is an alias only; no administrative state or ownership is adopted.")
		case "VRF":
			c.vrf(r)
		case "Bridge":
			c.bridge(r)
		case "Sysctl":
			c.sysctl(r)
		}
	}
	// All interface ownership declarations have been collected before any
	// address or member can request its independently owned native settings.
	for _, r := range resources {
		switch r.Kind {
		case "IPv4StaticAddress":
			nodes := c.static4(r)
			for _, node := range nodes {
				conditions = append(conditions, resourceCondition{id: node.ID, source: r, when: r.Spec.(api.IPv4StaticAddressSpec).When})
			}
			extra = append(extra, nodes...)
		case "VirtualAddress":
			name, nodes := c.virtualAddress(r)
			if name != "" {
				vrrpNames[r.Kind+"/"+r.Metadata.Name] = name
			}
			if source := r.Spec.(api.VirtualAddressSpec); source.Mode != "vrrp" {
				for _, node := range nodes {
					conditions = append(conditions, resourceCondition{id: node.ID, source: r, when: source.When})
				}
			}
			extra = append(extra, nodes...)
		case "DHCPv4Client":
			c.dhcp4(r)
		case "DHCPv6PrefixDelegation":
			name := c.dhcp6(r)
			conditions = append(conditions, resourceCondition{id: "service/" + name, source: r, when: r.Spec.(api.DHCPv6PrefixDelegationSpec).When})
		case "IPv6RAAddress":
			c.raClient(r)
		}
	}
	c.implicitRAClients()
	for _, r := range resources {
		switch r.Kind {
		case "IPv6DelegatedAddress":
			c.delegation(r)
		case "DHCPv6Information":
			if name := c.dhcp6Information(r); name != "" {
				informationConditions = append(informationConditions, resourceCondition{id: "service/" + name, source: r, when: r.Spec.(api.DHCPv6InformationSpec).When})
			}
		}
	}
	// AFTR references may point to a PD information alias declared later in
	// the source. Compile all aliases before resolving the tunnel graph.
	for _, r := range resources {
		if r.Kind == "DSLiteTunnel" {
			when := c.dslite(r)
			if _, dynamic := c.dynamicTunnels[c.aliases[r.Kind+"/"+r.Metadata.Name]]; dynamic {
				conditions = append(conditions, resourceCondition{id: "service/" + c.serviceName(r, "aftr"), source: r, when: when})
			}
		}
	}
	for _, r := range resources {
		switch r.Kind {
		case "IPv4StaticRoute":
			s := r.Spec.(api.IPv4StaticRouteSpec)
			c.allow(r, "destination", "via", "interface", "metric")
			c.route(r, network.Route{Destination: s.Destination, Gateway: s.Via, Interface: s.Interface, Metric: s.Metric}, 4, false)
		case "IPv6StaticRoute":
			s := r.Spec.(api.IPv6StaticRouteSpec)
			c.allow(r, "destination", "via", "interface", "metric")
			c.route(r, network.Route{Destination: s.Destination, Gateway: s.Via, Interface: s.Interface, Metric: s.Metric}, 6, false)
		case "IPv4Route":
			c.ipv4Route(r)
		case "DSLiteTunnel":
			// Compiled before routes so dynamic tunnel ownership can be joined.
		case "TailscaleNode":
			c.tailscale(r)
		case "Telemetry":
			c.telemetry(r)
		case "LogSink":
			c.logSink(r)
		case "ManagementAccess":
			// Checked together at the import boundary above.
		case "LogRetention":
			c.logRetention(r)
		case "HealthCheck":
			name := c.healthCheck(r)
			conditions = append(conditions, resourceCondition{id: "service/" + name, source: r, when: r.Spec.(api.HealthCheckSpec).When})
		case "IPv6RouterAdvertisement":
			// Compiled with delegated LAN advertisements below.
		case "FirewallEventLog", "FirewallZone", "FirewallPolicy", "FirewallRule", "NAT44Rule", "IPAddressSet", "LocalServiceRedirect":
			// Compiled together to preserve ordered zone jumps and NAT-only hooks.
		case "IngressService":
			// Compiled after the existing filter and NAT intent below.
		case "EgressRoutePolicy":
			if r.Spec.(api.EgressRoutePolicySpec).HostTraffic {
				extra = append(extra, c.hostRoute(r, vrrpNames))
			} else {
				extra = append(extra, c.egressPolicy(r, vrrpNames))
			}
		case "VXLANSegment", "VXLANTunnel":
			c.reject(r, "", "unsupported_semantics", "legacy VXLAN FDB peers, L2 filtering and outer-DF defaults require explicit native conversion; a single remote is not a lossless substitute")
		case "NAT44SessionSync":
			// Joined to the single IPv4 election after NAT conditions are compiled.
		case "NTPClient", "NTPServer":
			// Joined below into the one clock owner.
		case "SAMNodeSet", "SAMTransportProfile":
			// Static IPIP topology is joined to native DHCP/BGP ownership below.
		case "BGPRouter", "BGPPeer":
			// Compiled together with inherited routing policies below.
		case "DNSResolver", "DNSForwarder", "DNSUpstream", "DNSZone":
			// Compiled together below so graph references are checked.
		case "DHCPv4Server", "DHCPv4Reservation", "DHCPv4Relay", "DHCPv6Server", "DHCPv4ServerLeaseSync", "DHCPv6PrefixDelegationLeaseSync", "DHCPv6ServerLeaseSync":
			// Compile the complete static server/reservation graph before DNS.
		case "Interface", "Link", "VRF", "Bridge", "Sysctl", "IPv4StaticAddress",
			"WireGuardInterface", "WireGuardPeer",
			"VirtualAddress", "DHCPv4Client", "DHCPv6PrefixDelegation",
			"IPv6RAAddress", "IPv6DelegatedAddress", "DHCPv6Information":
		default:
			c.reject(r, "", "unsupported_kind", "no native migration is implemented for this resource kind; the resource was not discarded")
		}
	}
	servingConditions := c.routerAdvertisements(vrrpNames)
	for id, when := range c.dhcp6Servers(vrrpNames) {
		servingConditions[id] = when
	}
	c.dhcpServers(vrrpNames)
	c.leaseSync(vrrpNames)
	c.dhcpRelays()
	c.dns()
	conditions = append(conditions, c.bgp()...)
	transports, bgpDependencies := c.ipipTransports()
	extra = append(extra, transports...)
	c.ntp(vrrpNames)
	extra = append(extra, c.firewall(vrrpNames)...)
	c.conntrackSync(extra)
	c.localServiceRedirects()
	c.firewallServices()
	c.vpnListenerFirewall()
	extra = append(extra, c.ingressServices(vrrpNames)...)
	// Admission references can name a PD declared after its VRRP election.
	for _, r := range resources {
		s, ok := r.Spec.(api.VirtualAddressSpec)
		if !ok || s.VRRP.GracefulActivation == nil {
			continue
		}
		activation := s.VRRP.GracefulActivation
		if len(activation.ReadyWhen.All) == 0 && len(activation.ReadyWhen.Any) == 0 && len(activation.ReadyWhen.State) == 0 {
			c.reject(r, "vrrp.gracefulActivation.readyWhen", "invalid_value", "graceful activation requires a readiness expression")
			continue
		}
		when := c.when(r, activation.ReadyWhen, vrrpNames)
		if when == nil {
			continue // A nonempty constant-true expression needs no gate.
		}
		for i := range c.document.Services {
			service := &c.document.Services[i]
			if service.VRRP != nil && service.Name == vrrpNames[r.Kind+"/"+r.Metadata.Name] {
				service.VRRP.GracefulActivation = &vrrp.GracefulActivation{ReadyWhen: *when, Timeout: activation.Timeout}
				break
			}
		}
	}
	c.routingDefaults()
	c.checkOptionsUsed()
	c.checkAddressOverlap()
	for _, iface := range c.interfaces {
		c.document.Network.Interfaces = append(c.document.Network.Interfaces, *iface)
	}
	sort.Slice(c.document.Network.Interfaces, func(i, j int) bool {
		return c.document.Network.Interfaces[i].Name < c.document.Network.Interfaces[j].Name
	})
	sort.Slice(c.document.Services, func(i, j int) bool { return c.document.Services[i].Name < c.document.Services[j].Name })
	program := config.FromNative(c.document)
	program.Retention = c.retention
	program.Resources = append(program.Resources, extra...)
	for i := range program.Resources {
		if deps := bgpDependencies[program.Resources[i].ID]; len(deps) > 0 {
			program.Resources[i].DependsOn = append(program.Resources[i].DependsOn, deps...)
		}
	}
	for i := range program.Resources {
		if when := servingConditions[program.Resources[i].ID]; when != nil {
			program.Resources[i].When = when
		}
	}
	for _, condition := range conditions {
		when := c.when(condition.source, condition.when, vrrpNames)
		if when == nil {
			continue
		}
		for i := range program.Resources {
			if program.Resources[i].ID == condition.id {
				program.Resources[i].When = when
				break
			}
		}
	}
	// A probe bound to a generated tunnel starts only after the tunnel owner
	// has a current endpoint. It must not create an unconditional interface.
	for i := range program.Resources {
		resource := &program.Resources[i]
		if resource.Service == nil || resource.Service.Health == nil {
			continue
		}
		for _, provider := range c.document.Services {
			if provider.DSLite != nil && provider.DSLite.Tunnel == resource.Service.Health.Interface {
				resource.DependsOn = append(resource.DependsOn, "service/"+provider.Name)
				resource.When = egressAll(resource.When, &resourceprogram.FactCondition{Fact: provider.ModuleName() + ".active"})
				break
			}
		}
	}
	// Information shares its PD client's socket and information lease. An
	// unconditional alias or the same guard adds no independent lifetime.
	for _, condition := range informationConditions {
		when := c.when(condition.source, condition.when, vrrpNames)
		if when == nil {
			continue
		}
		for _, resource := range program.Resources {
			if resource.ID == condition.id && !reflect.DeepEqual(when, resource.When) {
				c.reject(condition.source, "when", "unsupported_semantics", "information sharing a PD socket must have the same condition as its PD owner")
			}
		}
	}
	// HA ownership chooses serving versus receiving state. Keep the worker
	// running on backup; aliases above still compare the original conditions.
	for i := range program.Resources {
		service := program.Resources[i].Service
		if service != nil && (service.DHCP6 != nil && service.DHCP6.HA != nil || service.DHCPServer != nil && service.DHCPServer.HA != nil) {
			program.Resources[i].When = nil
		}
	}
	if len(c.issues) == 0 {
		if err := config.Validate(program); err != nil {
			// Native validators sometimes include the invalid value. The
			// compatibility boundary must not print a URL credential or key.
			c.issue(nil, "$", "native_validation", "translated intent does not satisfy native validation; inspect the field restrictions in the migration documentation")
		}
	}
	if err := c.err(); err != nil {
		return Result{}, err
	}
	result := Result{Document: program, SourceRouter: raw.Metadata.Name, SourceRevision: SourceRevision}
	for _, r := range resources {
		m := c.mappings[r.ID()]
		if len(m.Targets) == 0 {
			m.Targets = []string{"reference-only"}
		}
		result.Mappings = append(result.Mappings, *m)
	}
	return result, nil
}

type staticPrefix struct {
	resource resource
	iface    string
	prefix   netip.Prefix
	allow    bool
}

type compiler struct {
	retention      *retention.Config
	queryRetention *dnsconfig.QueryRetention
	*decoder
	resources      []resource
	legacy         api.Router
	document       config.Native
	byRef          map[string]*resource
	aliases        map[string]string
	aliasOwner     map[string]string
	upAllowed      map[string]bool
	interfaces     map[string]*network.Interface
	names          map[string]string
	pd             map[string]int
	raClients      map[string]int
	information    map[string]int
	dynamicTunnels map[string]int
	dynamicRoutes  map[string]bool
	options        Options
	policyPaths    map[string]string
	policyUsed     map[string]bool
	mappings       map[string]*Mapping
	static         []staticPrefix
}

func (c *compiler) reject(r resource, field, code, message string) {
	path := r.path + ".spec"
	node := r.node
	if field != "" {
		path += "." + field
		// Nested paths may not have a direct node; the spec line is still useful.
		if n := child(r.node, field); n != nil {
			node = n
		}
	}
	c.issue(node, path, code, message)
}

func (c *compiler) allow(r resource, fields ...string) {
	allowed := map[string]bool{"when": true, "dependsOn": true, "ready_when": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for i := 0; i+1 < len(r.node.Content); i += 2 {
		field := r.node.Content[i].Value
		if !allowed[field] {
			c.reject(r, safeField(field), "unsupported_field", "this field has no implemented native migration")
		}
	}
	for _, field := range []string{"when", "dependsOn", "ready_when"} {
		if field == "when" {
			switch r.Kind {
			case "DHCPv4Server", "DHCPv4Reservation", "DHCPv4ServerLeaseSync", "DHCPv6PrefixDelegationLeaseSync", "DHCPv6ServerLeaseSync", "IPv4StaticAddress", "NAT44Rule", "BGPRouter", "EgressRoutePolicy", "HealthCheck", "DHCPv6PrefixDelegation", "DHCPv6Information", "NTPClient", "DHCPv6Server", "IPv6RouterAdvertisement":
				continue
			case "DSLiteTunnel":
				if _, dynamic := c.options.DSLite[r.Kind+"/"+r.Metadata.Name]; dynamic || r.Spec.(api.DSLiteTunnelSpec).LocalAddressSource == "delegatedAddress" {
					continue
				}
			case "VirtualAddress":
				if r.Spec.(api.VirtualAddressSpec).Mode != "vrrp" {
					continue
				}
			}
		}
		if field == "dependsOn" {
			if r.Kind == "DHCPv6Information" || r.Kind == "IPv6DelegatedAddress" || r.Kind == "DHCPv6Server" || r.Kind == "IPv6RouterAdvertisement" {
				continue // Checked against the allocation's PD owner.
			}
			if r.Kind == "DSLiteTunnel" {
				if _, dynamic := c.options.DSLite[r.Kind+"/"+r.Metadata.Name]; dynamic || r.Spec.(api.DSLiteTunnelSpec).LocalAddressSource == "delegatedAddress" {
					continue // Checked against the tunnel's actual source.
				}
			}
		}
		if meaningful(child(r.node, field)) {
			c.reject(r, field, "unsupported_semantics", "runtime gating/dependency conditions cannot be erased by a static migration")
		}
	}
}

func (c *compiler) target(r resource, target string) {
	m := c.mappings[r.ID()]
	for _, old := range m.Targets {
		if old == target {
			return
		}
	}
	m.Targets = append(m.Targets, target)
}

func (c *compiler) note(r resource, note string) {
	c.mappings[r.ID()].Notes = append(c.mappings[r.ID()].Notes, note)
}

func (c *compiler) serviceName(r resource, role string) string {
	id := r.Kind + "/" + r.Metadata.Name
	sum := sha256.Sum256([]byte(id))
	suffix := hex.EncodeToString(sum[:3])
	var slug strings.Builder
	for _, ch := range strings.ToLower(r.Metadata.Name) {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' {
			slug.WriteRune(ch)
		} else {
			slug.WriteByte('-')
		}
	}
	s := strings.Trim(slug.String(), "-")
	limit := 24 - len(role) - len(suffix) - 2
	if len(s) > limit {
		s = strings.Trim(s[:limit], "-")
	}
	if s == "" {
		s = "r"
	}
	name := role + "-" + s + "-" + suffix
	if other, ok := c.names[name]; ok && other != id {
		c.reject(r, "", "identity_collision", "derived native service identity collides with another resource")
	}
	c.names[name] = id
	return name
}

func (c *compiler) addService(r resource, service serviceconfig.Service) {
	c.document.Services = append(c.document.Services, service)
	c.target(r, "services/"+service.Name)
}

func (c *compiler) getRef(r resource, field, value, kind string) *resource {
	name := value
	if k, n, found := strings.Cut(value, "/"); found {
		if k != kind || strings.Contains(n, "/") {
			c.reject(r, field, "reference", "reference must name the expected kind and one resource name")
			return nil
		}
		name = n
	}
	result := c.byRef[kind+"/"+name]
	if result == nil {
		c.reject(r, field, "reference", "referenced resource is missing or invalid")
	}
	return result
}

func (c *compiler) resolveStaticAddress(r resource, field string, source api.StatusValueSourceSpec, ipv4 bool) string {
	address, when := c.resolveAddress(r, field, source, ipv4)
	if len(when.All)+len(when.Any)+len(when.State) != 0 {
		c.reject(r, field, "unsupported_semantics", "this static consumer cannot retain the source address activation condition")
		return ""
	}
	return address
}

// A literal address and its activation are separate intent. Consumers which
// retain activation can prepare the value without publishing it unconditionally.
func (c *compiler) resolveAddress(r resource, field string, source api.StatusValueSourceSpec, ipv4 bool) (string, api.ResourceWhenSpec) {
	if source.Optional {
		c.reject(r, field, "unsupported_semantics", "optional status-source fallback cannot be frozen into a static address")
		return "", api.ResourceWhenSpec{}
	}
	if source.Field != "" && source.Field != "address" {
		c.reject(r, field+".field", "unsupported_semantics", "only an explicit static resource address can be resolved during migration")
		return "", api.ResourceWhenSpec{}
	}
	ref := c.byRef[source.Resource]
	if ref == nil {
		c.reject(r, field+".resource", "reference", "static address source is missing or not a Kind/name reference")
		return "", api.ResourceWhenSpec{}
	}
	value := ""
	var when api.ResourceWhenSpec
	switch s := ref.Spec.(type) {
	case api.IPv4StaticAddressSpec:
		if ipv4 {
			value, when = s.Address, s.When
		}
	case api.VirtualAddressSpec:
		if (s.Mode == "" || s.Mode == "static") && s.AddressFrom.Resource == "" {
			value, when = s.Address, s.When
		}
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Addr().Is4() != ipv4 || prefix.Addr().Is4In6() {
		c.reject(r, field, "unsupported_semantics", "address source is dynamic, invalid, or of the wrong address family")
		return "", api.ResourceWhenSpec{}
	}
	return prefix.Addr().String(), when
}

func (c *compiler) checkAddressOverlap() {
	for i, left := range c.static {
		for _, right := range c.static[:i] {
			if left.iface == right.iface && left.prefix == right.prefix {
				c.reject(left.resource, "address", "duplicate_address", "two legacy resources claim the same interface address")
				continue
			}
			if left.iface != right.iface && left.prefix.Overlaps(right.prefix) && !left.allow && !right.allow {
				c.reject(left.resource, "allowOverlap", "address_overlap", "overlapping IPv4 networks on different interfaces require the original explicit overlap policy")
			}
		}
	}
}
