package resourceprogram

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// Compile validates the entire program before returning any executable output.
// Dependency order is deterministic for the same declarations regardless of
// their order in YAML. Payload order inside a resource (e.g. firewall rules) is
// preserved. Source and returned values never share mutable slices or pointers.
func Compile(source Program) (Plan, error) {
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
	encoded, err := json.Marshal(source)
	if err != nil {
		return Plan{}, fmt.Errorf("encode resource program: %w", err)
	}
	var program Program
	if err := json.Unmarshal(encoded, &program); err != nil {
		return Plan{}, fmt.Errorf("clone resource program: %w", err)
	}
	byID := make(map[string]Resource, len(program.Resources))
	for _, r := range program.Resources {
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
		if p := r.Provenance; p != nil {
			for _, s := range []string{p.Source, p.Resource, p.Revision, p.Path} {
				if len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
					return Plan{}, fmt.Errorf("resource %s: provenance values must be UTF-8, at most 4096 bytes and contain no NUL or newline", r.ID)
				}
			}
		}
		byID[r.ID] = r
	}
	dependencies, err := dependencyGraph(program.Resources, byID)
	if err != nil {
		return Plan{}, err
	}
	order, err := dependencyOrder(dependencies)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Order: order, Dependencies: dependencies}
	interfaces := map[string]int{}
	claimed := map[string]string{}
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
	ensureInterface := func(name string) int {
		if index, exists := interfaces[name]; exists {
			return index
		}
		index := len(plan.Native.Network.Interfaces)
		interfaces[name] = index
		plan.Native.Network.Interfaces = append(plan.Native.Network.Interfaces, network.Interface{Name: name})
		return index
	}
	for _, id := range order {
		r := byID[id]
		var err error
		switch {
		case r.Interface != nil:
			err = claim(r, "interface", r.Interface.Name)
			index := ensureInterface(r.Interface.Name)
			previous := plan.Native.Network.Interfaces[index]
			plan.Native.Network.Interfaces[index] = *r.Interface
			plan.Native.Network.Interfaces[index].Addresses = previous.Addresses
			plan.Native.Network.Interfaces[index].AddressOptions = previous.AddressOptions
		case r.Address != nil:
			key := canonicalPrefix(r.Address.CIDR)
			err = claim(r, "address", r.Address.Interface+"/"+key)
			index := ensureInterface(r.Address.Interface)
			iface := &plan.Native.Network.Interfaces[index]
			iface.Addresses = append(iface.Addresses, r.Address.CIDR)
			if r.Address.Options != nil {
				if iface.AddressOptions == nil {
					iface.AddressOptions = map[string]network.AddressOptions{}
				}
				iface.AddressOptions[r.Address.CIDR] = *r.Address.Options
			}
		case r.Link != nil:
			err = claim(r, "link", r.Link.Name)
			plan.Native.Network.Links = append(plan.Native.Network.Links, *r.Link)
		case r.Route != nil:
			err = claim(r, "route", routeKey(*r.Route))
			plan.Native.Network.Routes = append(plan.Native.Network.Routes, *r.Route)
		case r.PolicyRule != nil:
			err = claim(r, "policy_rule", fmt.Sprintf("%d/%d", r.PolicyRule.Family, r.PolicyRule.Priority))
			plan.Native.Network.PolicyRules = append(plan.Native.Network.PolicyRules, *r.PolicyRule)
		case r.Forwarding != nil:
			if r.Forwarding.IPv4 == nil && r.Forwarding.IPv6 == nil {
				err = fmt.Errorf("resource %s: forwarding requires ipv4 or ipv6", r.ID)
			}
			if r.Forwarding.IPv4 != nil {
				err = errors.Join(err, claim(r, "forwarding", "ipv4"))
				plan.Native.Network.Forwarding.IPv4 = r.Forwarding.IPv4
			}
			if r.Forwarding.IPv6 != nil {
				err = errors.Join(err, claim(r, "forwarding", "ipv6"))
				plan.Native.Network.Forwarding.IPv6 = r.Forwarding.IPv6
			}
		case r.Firewall != nil:
			err = claim(r, "firewall", network.FirewallTable)
			plan.Native.Network.Firewall = r.Firewall
		case r.DSLite != nil:
			err = claim(r, "link", r.DSLite.Name)
			plan.Native.Network.DSLite = append(plan.Native.Network.DSLite, *r.DSLite)
		case r.VRRPGuard != nil:
			for _, a := range r.VRRPGuard.Addresses {
				if ip, parseErr := netip.ParseAddr(a); parseErr == nil {
					a = ip.String()
				}
				err = errors.Join(err, claim(r, "vrrp_guard", a))
			}
			plan.Native.Network.VRRPGuards = append(plan.Native.Network.VRRPGuards, *r.VRRPGuard)
		case r.WireGuard != nil:
			err = claim(r, "wireguard", r.WireGuard.Interface)
			plan.Native.Network.WireGuard = append(plan.Native.Network.WireGuard, *r.WireGuard)
		case r.Service != nil:
			err = claim(r, "service", r.Service.ModuleName())
			plan.Native.Services = append(plan.Native.Services, *r.Service)
		}
		if err != nil {
			return Plan{}, err
		}
	}
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
