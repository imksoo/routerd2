package resourceprogram

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/eventrule"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func ValidResourceID(id string) bool { return resourceID.MatchString(id) }

// Compile validates the entire program before returning any executable output.
// Dependency order is deterministic for the same declarations regardless of
// their order in YAML. Payload order inside a resource (e.g. firewall rules) is
// preserved. The caller keeps source payloads immutable for the lifetime of the plan;
// compilation does not serialize typed configuration to clone it.
func Compile(source Program) (Plan, error) {
	if source.Retention != nil {
		if err := source.Retention.Validate(); err != nil {
			return Plan{}, err
		}
	}
	for _, r := range source.Resources {
		for _, guard := range []struct {
			name  string
			value *FactCondition
		}{{"when", r.When}, {"ready_when", r.ReadyWhen}} {
			if guard.value != nil {
				if err := ValidateFactCondition(*guard.value); err != nil {
					return Plan{}, fmt.Errorf("resource %s %s: %w", r.ID, guard.name, err)
				}
			}
		}
	}
	if source.SchemaVersion != SchemaVersion {
		return Plan{}, fmt.Errorf("unsupported config schema %d (expected %d)", source.SchemaVersion, SchemaVersion)
	}
	byID := make(map[string]Resource, len(source.Resources))
	source.Resources = slices.Clone(source.Resources)
	for i, r := range source.Resources {
		if !resourceID.MatchString(r.ID) {
			return Plan{}, fmt.Errorf("invalid resource id %q (1..128 ASCII letters, digits, . _ : / -)", r.ID)
		}
		if _, exists := byID[r.ID]; exists {
			return Plan{}, fmt.Errorf("duplicate resource id %q", r.ID)
		}
		if _, count := r.kind(); count != 1 {
			return Plan{}, fmt.Errorf("resource %s must select exactly one payload", r.ID)
		}
		if r.Interface != nil && (len(r.Interface.Addresses) != 0 || len(r.Interface.AddressOptions) != 0) {
			return Plan{}, fmt.Errorf("resource %s: interface addresses and address_options belong in separate address resources", r.ID)
		}
		if r.Firewall != nil && len(r.Firewall.NAT) != 0 {
			return Plan{}, fmt.Errorf("resource %s: NAT rules belong in a nat resource", r.ID)
		}
		if r.NAT != nil {
			if len(r.NAT) == 0 {
				return Plan{}, fmt.Errorf("resource %s: nat requires at least one rule", r.ID)
			}
			for _, rule := range r.NAT {
				if rule.Authority != (lifetime.Deadline{}) {
					return Plan{}, fmt.Errorf("resource %s: NAT authority is internal projection authority", r.ID)
				}
			}
		}
		if r.Address != nil && r.Address.Options != nil && r.Address.Options.BootLifetime != (network.BootLifetime{}) {
			return Plan{}, fmt.Errorf("resource %s: boot lifetime is internal projection authority", r.ID)
		}
		if r.Link != nil && r.Link.Authority != (lifetime.Deadline{}) {
			return Plan{}, fmt.Errorf("resource %s: link authority is internal projection authority", r.ID)
		}
		if r.PolicyRule != nil && r.PolicyRule.Authority != (lifetime.Deadline{}) {
			return Plan{}, fmt.Errorf("resource %s: policy rule authority is internal projection authority", r.ID)
		}
		if r.Route != nil && (r.Route.BootID != "" || r.Route.ExpiresNanos != 0 || r.Route.ValidUntil.BootID != "") {
			return Plan{}, fmt.Errorf("resource %s: route boot deadline is internal projection authority", r.ID)
		}
		if p := r.Provenance; p != nil {
			for _, s := range []string{p.Source, p.Resource, p.Revision, p.Path} {
				if len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
					return Plan{}, fmt.Errorf("resource %s: provenance values must be UTF-8, at most 4096 bytes and contain no NUL or newline", r.ID)
				}
			}
		}
		if r.Service != nil {
			if ((r.Service.DHCPServer != nil && r.Service.DHCPServer.HA != nil) || (r.Service.DHCP6 != nil && r.Service.DHCP6.HA != nil)) && r.When != nil {
				return Plan{}, fmt.Errorf("DHCP %s HA service must remain running on standby; use ha.owner instead of when", r.ID)
			}
			s := serviceconfig.Normalize(*r.Service)
			r.Service = &s
			source.Resources[i] = r
		}
		byID[r.ID] = r
	}
	preparedIPIPs := make(map[string]IPIPPlan)
	for i, r := range source.Resources {
		if r.IPIP == nil {
			continue
		}
		ipip, provider, err := prepareIPIP(r.ID, *r.IPIP, source.Resources)
		if err != nil {
			return Plan{}, err
		}
		preparedIPIPs[r.ID] = ipip
		lease := FactCondition{Fact: ipip.Source + ".active"}
		if r.When == nil {
			r.When = &lease
		} else {
			r.When = &FactCondition{All: []FactCondition{*r.When, lease}}
		}
		if !slices.Contains(r.DependsOn, provider) {
			r.DependsOn = append(slices.Clone(r.DependsOn), provider)
		}
		source.Resources[i], byID[r.ID] = r, r
	}
	// Tracks are observations, not hard readiness dependencies. Resolve them
	// against declarations so conditional removal does not invalidate the VIP.
	for _, r := range source.Resources {
		if r.Service == nil || r.Service.VRRP == nil {
			continue
		}
		for _, track := range r.Service.VRRP.Track {
			if len(track.IngressHealth) > 0 {
				seen := map[string]bool{}
				for _, id := range track.IngressHealth {
					target, exists := byID[id]
					if !exists || target.Service == nil || target.Service.Health == nil || seen[id] {
						return Plan{}, fmt.Errorf("VRRP %s ingress track %s requires distinct declared Health providers", r.ID, track.Resource)
					}
					seen[id] = true
				}
				continue
			}
			target, exists := byID[track.Resource]
			if !exists || target.Service == nil || target.Service.Health == nil && target.Service.BFD == nil && target.Service.BGP == nil {
				return Plan{}, fmt.Errorf("VRRP %s track %s requires a declared Health, BFD or BGP status provider", r.ID, track.Resource)
			}
		}
	}
	dependencies, err := dependencyGraph(source.Resources, byID)
	if err != nil {
		return Plan{}, err
	}
	order, err := dependencyOrder(dependencies)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Order: order, Dependencies: dependencies}
	claimed := map[string]string{}
	derivedRefs := map[string]bool{}
	ruleCount, derivedCount := 0, 0
	noPathOwned := false
	claim := func(r Resource, kind, key string) error {
		identity := kind + "\x00" + key
		if previous, exists := claimed[identity]; exists {
			return fmt.Errorf("resources %s and %s both own %s %s", previous, r.ID, kind, key)
		}
		claimed[identity] = r.ID
		owner := r.Owner
		if owner == "" {
			owner = r.ID
		}
		plan.Ownership = append(plan.Ownership, Ownership{ResourceID: r.ID, OwnerID: owner, Kind: kind, Key: key, Provenance: r.Provenance})
		return nil
	}
	for _, id := range order {
		r := byID[id]
		node := compiledResource{source: r}
		var err error
		switch {
		case r.Interface != nil:
			err = claim(r, "interface", r.Interface.Name)
		case r.Address != nil:
			key := canonicalPrefix(r.Address.CIDR)
			err = claim(r, "address", r.Address.Interface+"/"+key)
		case r.IPIP != nil:
			ipip := preparedIPIPs[r.ID]
			err = errors.Join(claim(r, "interface", ipip.Link.Name), claim(r, "link", ipip.Link.Name), claim(r, "address", ipip.Link.Name+"/"+ipip.Address.String()))
			plan.IPIPs = append(plan.IPIPs, ipip)
		case r.Link != nil:
			err = claim(r, "link", r.Link.Name)
		case r.Route != nil:
			err = claim(r, "route", routeKey(*r.Route))
		case r.EgressRoute != nil:
			route, compileErr := r.EgressRoute.compile()
			err = compileErr
			if err == nil {
				err = claim(r, "route", routeKey(route))
				node.egress = &route
			}
		case r.HostRoute != nil:
			host, compileErr := prepareHostRoute(r.ID, *r.HostRoute)
			err = compileErr
			if host.RouterFrom != "" {
				found := false
				for _, provider := range source.Resources {
					if provider.Service != nil && provider.Service.ModuleName() == host.RouterFrom && provider.Service.RAClient != nil && provider.Service.Interface() == host.Route.Interface {
						found = true
						break
					}
				}
				if !found {
					return Plan{}, fmt.Errorf("host route %s router_from requires an RA client on its interface", r.ID)
				}
			}
			if err == nil {
				err = errors.Join(claim(r, "route", routeKey(host.Route)), claim(r, "policy_rule", fmt.Sprintf("6/%d", host.Rule.Priority)), claim(r, "policy_rule", fmt.Sprintf("6/%d", host.LocalRule.Priority)))
				for _, bypass := range host.Bypasses {
					found := false
					for _, provider := range source.Resources {
						if provider.Service != nil && provider.Service.ModuleName() == bypass.Service && provider.Service.DSLite != nil {
							found = true
							break
						}
					}
					if !found {
						return Plan{}, fmt.Errorf("host route %s bypass requires a declared DS-Lite service", r.ID)
					}
					if bypass.Priority < 1 || bypass.Priority >= host.Rule.Priority {
						return Plan{}, fmt.Errorf("host route %s bypass must precede its host rule", r.ID)
					}
					err = errors.Join(err, claim(r, "policy_rule", fmt.Sprintf("6/%d", bypass.Priority)))
				}
				plan.HostRoutes = append(plan.HostRoutes, host)
			}
		case r.EgressPolicy != nil:
			policy, compileErr := prepareEgressPolicy(r.ID, *r.EgressPolicy)
			err = compileErr
			if err == nil {
				for _, group := range policy.spec.Groups {
					for _, target := range group.Targets {
						err = errors.Join(err, claim(r, "route", routeKey(network.Route{Destination: "0.0.0.0/0", Table: target.Table, Metric: target.Metric})), claim(r, "policy_rule", fmt.Sprintf("4/%d", target.Priority)), claim(r, "flow_mark", fmt.Sprint(target.Mark)))
					}
				}
				for _, fallback := range policy.Fallbacks {
					err = errors.Join(err, claim(r, "route", routeKey(fallback)))
				}
				if len(policy.Flows) != 0 && !noPathOwned {
					err = errors.Join(err, claim(r, "policy_rule", fmt.Sprintf("4/%d", network.NoPathPriority)), claim(r, "flow_mark", fmt.Sprint(network.NoPathMark)))
					mark := network.NoPathMark
					policy.Rules = append(policy.Rules, network.PolicyRule{Family: 4, Priority: network.NoPathPriority, Mark: &mark, Blackhole: true})
					noPathOwned = true
				}
				plan.EgressPolicies = append(plan.EgressPolicies, policy)
			}
		case r.PolicyRule != nil:
			err = claim(r, "policy_rule", fmt.Sprintf("%d/%d", r.PolicyRule.Family, r.PolicyRule.Priority))
		case r.Forwarding != nil:
			if r.Forwarding.IPv4 == nil && r.Forwarding.IPv6 == nil && r.Forwarding.IPv4RPFilter == nil && r.Forwarding.ConntrackTCPLiberal == nil && r.Forwarding.IPv4ECMPHash == "" {
				err = fmt.Errorf("resource %s: forwarding requires at least one setting", r.ID)
			}
			if r.Forwarding.IPv4 != nil {
				err = errors.Join(err, claim(r, "forwarding", "ipv4"))
			}
			if r.Forwarding.IPv6 != nil {
				err = errors.Join(err, claim(r, "forwarding", "ipv6"))
			}
			if r.Forwarding.ConntrackTCPLiberal != nil {
				err = errors.Join(err, claim(r, "forwarding", "conntrack_tcp_liberal"))
			}
			if r.Forwarding.IPv4RPFilter != nil {
				err = errors.Join(err, claim(r, "forwarding", "ipv4_rp_filter"))
			}
			if r.Forwarding.IPv4ECMPHash != "" {
				err = errors.Join(err, claim(r, "forwarding", "ipv4_ecmp_hash"))
			}
		case r.Firewall != nil:
			err = claim(r, "firewall", network.FirewallTable)
		case r.NAT != nil:
			err = claim(r, "nat", r.ID)
		case r.VRRPGuard != nil:
			for _, a := range r.VRRPGuard.Addresses {
				if ip, parseErr := netip.ParseAddr(a); parseErr == nil {
					a = ip.String()
				}
				err = errors.Join(err, claim(r, "vrrp_guard", a))
			}
		case r.WireGuard != nil:
			err = claim(r, "wireguard", r.WireGuard.Interface)
		case r.Service != nil:
			err = claim(r, "service", r.Service.ModuleName())
		case r.EventRule != nil:
			ruleCount++
			if ruleCount > 128 {
				return Plan{}, errors.New("event rule resource count exceeds 128")
			}
			compiled, compileErr := eventrule.Compile(r.ID, *r.EventRule)
			err = compileErr
			if err == nil {
				err = claim(r, "event_rule", r.ID)
			}
			body, _ := json.Marshal(r)
			digest := sha256.Sum256(body)
			node.rule = EventRule{ID: r.ID, Identity: hex.EncodeToString(digest[:]), Spec: *r.EventRule, Compiled: compiled}
		case r.DerivedEvent != nil:
			derivedCount++
			if derivedCount > 128 {
				return Plan{}, errors.New("derived event resource count exceeds 128")
			}
			compiled, compileErr := derived.Compile(r.ID, *r.DerivedEvent)
			if compileErr != nil {
				return Plan{}, fmt.Errorf("derived event %s: %w", r.ID, compileErr)
			}
			for _, ref := range compiled.References() {
				target, exists := byID[ref.Resource]
				supported := exists && target.Service != nil && ((ref.Kind == "HealthCheck" && target.Service.Health != nil && health.ScalarFieldSupported(ref.Field)) || (ref.Kind == "BFD" && target.Service.BFD != nil && bfd.StatusFieldSupported(ref.Field)) || (ref.Kind == "BGPRouter" && target.Service.BGP != nil && bgpconfig.StatusFieldSupported(ref.Field)))
				if !supported {
					return Plan{}, fmt.Errorf("derived event %s has unsupported or missing typed reference %s", r.ID, ref.Key())
				}
				derivedRefs[ref.Key()] = true
				if len(derivedRefs) > 1024 {
					return Plan{}, errors.New("derived status references exceed 1024")
				}
			}
			err = claim(r, "derived_event", r.ID)
			body, _ := json.Marshal(r)
			sum := sha256.Sum256(body)
			node.derived = DerivedEvent{ID: r.ID, Identity: hex.EncodeToString(sum[:]), Spec: *r.DerivedEvent, References: compiled.References(), Compiled: compiled}
		}
		if err != nil {
			return Plan{}, err
		}
		plan.nodes = append(plan.nodes, node)
	}
	plan.Native = renderNative(plan.nodes)
	if err := errors.Join(network.Validate(plan.Native.Network), serviceconfig.Validate(plan.Native.Services)); err != nil {
		return Plan{}, fmt.Errorf("resource projection: %w", err)
	}
	return plan, nil
}

func canonicalPrefix(s string) string {
	if prefix, err := netip.ParsePrefix(s); err == nil {
		return prefix.String()
	}
	return s
}

func routeKey(route network.Route) string {
	table, metric := route.Table, route.Metric
	if table == 0 {
		table = 254
	}
	destination := route.Destination
	if p, err := netip.ParsePrefix(destination); err == nil {
		destination = p.Masked().String()
		if p.Addr().Is6() && metric == 0 {
			metric = 1024
		}
	}
	return fmt.Sprintf("%d/%s/%d", table, destination, metric)
}

func dependencyGraph(resources []Resource, byID map[string]Resource) (map[string][]string, error) {
	graph := make(map[string][]string, len(resources))
	for _, r := range resources {
		seen := map[string]bool{}
		for _, dep := range r.DependsOn {
			if _, exists := byID[dep]; !exists {
				return nil, fmt.Errorf("resource %s depends on missing resource %s", r.ID, dep)
			}
			if dep == r.ID || seen[dep] {
				return nil, fmt.Errorf("resource %s has a self or duplicate dependency %s", r.ID, dep)
			}
			seen[dep] = true
		}
		if r.Owner != "" && r.Owner != r.ID {
			if _, exists := byID[r.Owner]; !exists {
				return nil, fmt.Errorf("resource %s has missing owner %s", r.ID, r.Owner)
			}
		}
		graph[r.ID] = slices.Clone(r.DependsOn)
		if r.Owner != "" && r.Owner != r.ID && !slices.Contains(graph[r.ID], r.Owner) {
			graph[r.ID] = append(graph[r.ID], r.Owner)
		}
	}
	inferred := inferredDependencies(resources)
	for id, dependencies := range inferred {
		set := make(map[string]bool, len(graph[id])+len(dependencies))
		for _, dependency := range graph[id] {
			set[dependency] = true
		}
		for _, dependency := range dependencies {
			if dependency != id {
				set[dependency] = true
			}
		}
		graph[id] = graph[id][:0]
		for dependency := range set {
			graph[id] = append(graph[id], dependency)
		}
	}
	for id := range graph {
		slices.Sort(graph[id])
	}
	return graph, nil
}

// A lexical priority queue gives stable ordering without repeatedly scanning
// the complete program; large route sets compile in O((V+E) log V).
type idQueue []string

func (q idQueue) Len() int           { return len(q) }
func (q idQueue) Less(i, j int) bool { return q[i] < q[j] }
func (q idQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *idQueue) Push(v any)        { *q = append(*q, v.(string)) }
func (q *idQueue) Pop() any {
	old := *q
	v := old[len(old)-1]
	*q = old[:len(old)-1]
	return v
}

func dependencyOrder(graph map[string][]string) ([]string, error) {
	pending := make(map[string]int, len(graph))
	dependents := make(map[string][]string, len(graph))
	ready := &idQueue{}
	for id, dependencies := range graph {
		pending[id] = len(dependencies)
		if len(dependencies) == 0 {
			heap.Push(ready, id)
		}
		for _, dependency := range dependencies {
			dependents[dependency] = append(dependents[dependency], id)
		}
	}
	order := make([]string, 0, len(graph))
	for ready.Len() != 0 {
		id := heap.Pop(ready).(string)
		order = append(order, id)
		for _, dependent := range dependents[id] {
			pending[dependent]--
			if pending[dependent] == 0 {
				heap.Push(ready, dependent)
			}
		}
	}
	if len(order) != len(graph) {
		var blocked []string
		for id, n := range pending {
			if n != 0 {
				blocked = append(blocked, id)
			}
		}
		slices.Sort(blocked)
		const maximumReported = 16
		detail := strings.Join(blocked[:min(len(blocked), maximumReported)], ", ")
		if len(blocked) > maximumReported {
			detail += fmt.Sprintf(" (and %d more resources)", len(blocked)-maximumReported)
		}
		return nil, fmt.Errorf("resource dependency/owner cycle blocks %s", detail)
	}
	return order, nil
}
