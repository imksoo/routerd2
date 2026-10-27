// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"encoding"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"sort"
	"strings"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"gopkg.in/yaml.v3"
)

const OptionsSchemaVersion = 1

type TailscaleEnrollment string

const (
	TailscaleKeyFile       TailscaleEnrollment = "key_file"
	TailscaleSavedIdentity TailscaleEnrollment = "saved_identity"
)

// TailscalePolicy selects the new daemon's kernel and bootstrap ownership.
// Enrollment is key_file or saved_identity; legacy environment files and
// inline keys are never read or copied into the native configuration.
type TailscalePolicy struct {
	Interface     string              `json:"interface"`
	BootstrapDNS  []netip.Addr        `json:"bootstrap_dns,omitempty"`
	ControlCAFile string              `json:"control_ca_file"`
	Enrollment    TailscaleEnrollment `json:"enrollment"`
	AuthKeyFile   string              `json:"auth_key_file,omitempty"`
}

// Options contains explicit replacement intent, not switches to ignore errors.
// Map keys are exact legacy Kind/name identities. Each entry must be consumed by
// that one resource; unqualified aliases and extra entries are rejected.
type Options struct {
	Tailscale     map[string]TailscalePolicy        `json:"tailscale,omitempty"`
	SchemaVersion int                               `json:"schema_version"`
	RAClients     map[string]serviceconfig.RAClient `json:"ra_clients,omitempty"`
	DSLite        map[string]DSLitePolicy           `json:"ds_lite,omitempty"`
	DHCPDNS       map[string]DHCPDNSPolicy          `json:"dhcp_dns,omitempty"`
	DHCP6Servers  map[string]DHCP6ServerPolicy      `json:"dhcp6_servers,omitempty"`
}

// DHCP6ServerPolicy replaces dnsmasq's implicit interface-prefix constructor
// and retained options with an explicitly selected PD allocation and bounded
// native information. It does not override the legacy advertised values.
type DHCP6ServerPolicy struct {
	DelegatedAddress      string `json:"delegated_address"`
	RouterLifetimeSeconds *int   `json:"router_lifetime_seconds"`
}

// DHCPDNSPolicy explicitly replaces the old resolver's global lease-event and
// sticky-file behavior with configured source allowlists and finite producer/
// lease ownership. Source names and zone TTLs still come from the legacy graph.
type DHCPDNSPolicy struct {
	SourceScopedFiniteLeases *bool `json:"source_scoped_finite_leases"`
}

// DSLitePolicy replaces the old process-local DNS dependency and indefinitely
// retained pending tunnel with the native bounded AFTR contract. Endpoints,
// candidate order, tunnel/B4 identity, MTU and route intent come from the legacy
// resource graph, never from this policy.
type DSLitePolicy struct {
	SourceFrom     string                       `json:"source_from,omitempty"`
	DNSFrom        []string                     `json:"dns_from,omitempty"`
	Resolvers      []serviceconfig.AFTRResolver `json:"resolvers,omitempty"`
	Selection      string                       `json:"selection"`
	Ordinal        int                          `json:"ordinal,omitempty"`
	TimeoutSeconds int                          `json:"timeout_seconds"`
	RetrySeconds   int                          `json:"retry_seconds"`
}

// DecodeOptions reads one bounded, strict YAML/JSON policy document. It does not
// resolve resource references; CompileWithOptions checks those against the exact
// source document. Parsing errors do not expose policy scalars or raw key text.
func DecodeOptions(r io.Reader) (Options, error) {
	var options Options
	data, readErr := io.ReadAll(io.LimitReader(r, config.MaxSize+1))
	if readErr != nil || len(data) > config.MaxSize {
		return Options{}, &UnsupportedError{Issues: []Issue{{Path: "$policy", Code: "policy_decode", Message: "migration policy must be readable and at most 2 MiB"}}}
	}
	if err := config.Decode(bytes.NewReader(data), &options); err != nil {
		return Options{}, &UnsupportedError{Issues: []Issue{{Path: "$policy", Code: "policy_decode", Message: "expected one strict YAML/JSON migration policy of at most 2 MiB; unknown fields, duplicate keys, aliases and invalid types are rejected"}}}
	}
	d := &decoder{}
	// encoding/json accepts case-insensitive struct fields. The policy contract
	// requires the documented exact names, and null cannot stand for an override
	// object. The ordinary native decoder already bounded/checked the YAML tree.
	var tree yaml.Node
	if err := yaml.Unmarshal(data, &tree); err != nil || len(tree.Content) != 1 {
		d.issue(nil, "$policy", "policy_decode", "migration policy must contain one mapping")
	} else {
		checkPolicyFields(d, tree.Content[0], reflect.TypeOf(options), "$policy")
	}
	validateOptionsShape(d, options, true)
	if err := d.err(); err != nil {
		return Options{}, err
	}
	return options, nil
}

func checkPolicyFields(d *decoder, n *yaml.Node, typ reflect.Type, path string) {
	if n.Kind == yaml.MappingNode && n.Tag != "!!map" || n.Kind == yaml.SequenceNode && n.Tag != "!!seq" {
		d.issue(n, path, "policy_decode", "custom YAML tags are not supported in migration policy")
		return
	}
	for typ.Kind() == reflect.Pointer {
		if n.Tag == "!!null" {
			return
		}
		typ = typ.Elem()
	}
	// config.Decode already interpreted scalar text codecs (netip.Addr).
	// Their private Go fields are not a nested policy mapping.
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			d.issue(n, path, "policy_decode", "policy and resource overrides must be mappings")
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.PkgPath == "" && name != "-" {
				if name == "" {
					name = f.Name
				}
				fields[name] = f.Type
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			field, ok := fields[n.Content[i].Value]
			if !ok {
				d.issue(n.Content[i], path+".<field>", "policy_decode", "policy field names must match the documented schema exactly")
				continue
			}
			checkPolicyFields(d, n.Content[i+1], field, path+"."+n.Content[i].Value)
		}
	case reflect.Map:
		for i := 1; i < len(n.Content); i += 2 {
			checkPolicyFields(d, n.Content[i], typ.Elem(), fmt.Sprintf("%s[%d]", path, i/2))
		}
	case reflect.Slice:
		for i, node := range n.Content {
			checkPolicyFields(d, node, typ.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func validateOptionsShape(d *decoder, options Options, explicit bool) {
	if explicit && options.SchemaVersion != OptionsSchemaVersion {
		d.issue(nil, "$policy.schema_version", "policy_version", "migration policy schema_version must be 1")
	}
	if len(options.RAClients)+len(options.DSLite)+len(options.DHCPDNS)+len(options.DHCP6Servers)+len(options.Tailscale) > 4096 {
		d.issue(nil, "$policy", "policy_limit", "migration policy accepts at most 4096 resource entries")
	}
}

func (c *compiler) initOptions(options Options, explicit bool) {
	validateOptionsShape(c.decoder, options, explicit)
	c.policyPaths, c.policyUsed = map[string]string{}, map[string]bool{}
	if len(options.RAClients)+len(options.DSLite)+len(options.DHCPDNS)+len(options.DHCP6Servers)+len(options.Tailscale) > 4096 {
		return
	}
	// Do not let a caller's later edit mutate a successfully compiled document.
	c.options = Options{SchemaVersion: options.SchemaVersion, RAClients: map[string]serviceconfig.RAClient{}, DSLite: map[string]DSLitePolicy{}, DHCPDNS: map[string]DHCPDNSPolicy{}, DHCP6Servers: map[string]DHCP6ServerPolicy{}, Tailscale: map[string]TailscalePolicy{}}
	for key, value := range options.RAClients {
		value.SLAAC, value.DefaultRoute = copyBool(value.SLAAC), copyBool(value.DefaultRoute)
		value.OnLink, value.RouteInformation = copyBool(value.OnLink), copyBool(value.RouteInformation)
		value.DNS = copyBool(value.DNS)
		value.AllowedRouters = append([]string(nil), value.AllowedRouters...)
		c.options.RAClients[key] = value
	}
	for key, value := range options.DSLite {
		value.DNSFrom = append([]string(nil), value.DNSFrom...)
		value.Resolvers = append([]serviceconfig.AFTRResolver(nil), value.Resolvers...)
		c.options.DSLite[key] = value
	}
	for key, value := range options.DHCPDNS {
		value.SourceScopedFiniteLeases = copyBool(value.SourceScopedFiniteLeases)
		c.options.DHCPDNS[key] = value
	}
	for key, value := range options.DHCP6Servers {
		if value.RouterLifetimeSeconds != nil {
			lifetime := *value.RouterLifetimeSeconds
			value.RouterLifetimeSeconds = &lifetime
		}
		c.options.DHCP6Servers[key] = value
	}
	for key, value := range options.Tailscale {
		value.BootstrapDNS = append([]netip.Addr(nil), value.BootstrapDNS...)
		c.options.Tailscale[key] = value
	}
	check := func(section, kind string, keys []string) {
		sort.Strings(keys)
		for i, key := range keys {
			id := policyID(section, key)
			path := fmt.Sprintf("$policy.%s[%d]", section, i)
			c.policyPaths[id] = path
			resource := c.byRef[key]
			if resource == nil || resource.Kind != kind || !strings.HasPrefix(key, kind+"/") || strings.Count(key, "/") != 1 {
				c.issue(nil, path, "policy_reference", "policy key must identify one existing resource of the policy's kind using its exact Kind/name")
				c.policyUsed[id] = true // invalid, not a second unused-entry issue
			}
		}
	}
	keys := make([]string, 0, len(options.RAClients))
	for key := range options.RAClients {
		keys = append(keys, key)
	}
	check("ra_clients", "IPv6RAAddress", keys)
	keys = make([]string, 0, len(options.DSLite))
	for key := range options.DSLite {
		keys = append(keys, key)
	}
	check("ds_lite", "DSLiteTunnel", keys)
	keys = make([]string, 0, len(options.DHCPDNS))
	for key := range options.DHCPDNS {
		keys = append(keys, key)
	}
	check("dhcp_dns", "DNSResolver", keys)
	keys = make([]string, 0, len(options.DHCP6Servers))
	for key := range options.DHCP6Servers {
		keys = append(keys, key)
	}
	check("dhcp6_servers", "DHCPv6Server", keys)
	keys = make([]string, 0, len(options.Tailscale))
	for key := range options.Tailscale {
		keys = append(keys, key)
	}
	check("tailscale", "TailscaleNode", keys)
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func policyID(section, ref string) string { return section + "\x00" + ref }

func (c *compiler) usePolicy(r resource, section string) {
	c.policyUsed[policyID(section, r.Kind+"/"+r.Metadata.Name)] = true
}

func (c *compiler) policyIssue(r resource, section, field, code, message string) {
	path := c.policyPaths[policyID(section, r.Kind+"/"+r.Metadata.Name)]
	if path == "" {
		path = "$policy." + section
	}
	if field != "" {
		path += "." + field
	}
	c.issue(nil, path, code, message)
}

func (c *compiler) checkOptionsUsed() {
	keys := make([]string, 0, len(c.policyPaths))
	for key := range c.policyPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !c.policyUsed[key] {
			c.issue(nil, c.policyPaths[key], "unused_policy", "policy entry was not consumed by its intended resource; extra overrides are not allowed")
		}
	}
}
