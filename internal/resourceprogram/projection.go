package resourceprogram

import (
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

// FactSnapshot is an immutable, single-instant view. All resources in one
// projection see the same producer epochs, scope and expiration boundary.
type FactSnapshot struct {
	Scope    FactScope
	NowNanos uint64
	WallTime time.Time // paired with NowNanos for explicit UTC lifetime bounds
	// Facts is owned by this snapshot. The caller transfers the map and
	// must not mutate it while any evaluation uses this view.
	Facts map[string]Fact
	held  map[string]timedTruth
}

func (s FactSnapshot) Value(key string) Truth {
	fact, ok := s.Facts[key]
	if !ok || s.NowNanos < fact.ObservedNanos || s.NowNanos >= fact.ExpiresNanos {
		return Indeterminate
	}
	return fact.Value
}

func (s FactSnapshot) evaluate(c *FactCondition) Truth {
	if c == nil {
		return True
	}
	if c.For != "" {
		return s.held[conditionIdentity(c)].value
	}
	if c.Fact != "" {
		if c.MaxAge != "" {
			age, err := time.ParseDuration(c.MaxAge)
			fact, ok := s.Facts[c.Fact]
			observed := fact.ObservedNanos
			if err != nil || age <= 0 || !ok || s.NowNanos < observed || s.NowNanos-observed > uint64(age) {
				return Indeterminate
			}
		}
		return s.Value(c.Fact)
	}
	if c.Not != nil {
		return s.evaluate(c.Not).Not()
	}
	operands := c.All
	if operands == nil {
		operands = c.Any
	}
	values := make([]Truth, len(operands))
	for i := range operands {
		values[i] = s.evaluate(&operands[i])
	}
	if c.All != nil {
		return All(values...)
	}
	return Any(values...)
}

// ResourceState describes the desired projection, not a kernel acknowledgement.
// ready_when gates dependents; it never prevents its own resource from starting.
type ResourceState struct {
	ActiveUntil lifetime.Deadline `json:"active_until,omitzero"`
	Active      bool              `json:"active"`
	Ready       Truth             `json:"ready"`
	When        Truth             `json:"when"`
	Reason      string            `json:"reason,omitempty"`
	Dependency  string            `json:"dependency,omitempty"`
}

// Project evaluates only observations and dependency readiness. Configuration
// validation, graph construction and protocol compilation belong to Compile.
func (p Plan) Project(facts FactSnapshot) (Native, map[string]ResourceState) {
	states := make(map[string]ResourceState, len(p.nodes))
	bounds := make(map[string]uint64, len(p.nodes))
	var selected []compiledResource
	for _, node := range p.nodes {
		r := node.source
		state := ResourceState{When: True}
		untilNanos := uint64(math.MaxUint64)
		if r.When != nil {
			state.When, untilNanos = facts.ConditionAuthority(*r.When)
		}
		gate := state.When
		if gate != True {
			state.Reason = conditionReason("when", gate)
			if facts.pendingHold(r.When) {
				state.Reason = "when_hold"
			}
		}
		for _, dep := range p.Dependencies[r.ID] {
			gate = All(gate, states[dep].Ready)
			untilNanos = min(untilNanos, bounds[dep])
			if state.When == True && states[dep].Ready != True && (state.Dependency == "" || state.Reason == "dependency_indeterminate" && states[dep].Ready == False) {
				state.Reason, state.Dependency = conditionReason("dependency", states[dep].Ready), dep
			}
		}
		var validUntil time.Time
		if r.Address != nil && r.Address.Options != nil {
			validUntil = r.Address.Options.ValidUntil
		}
		if r.Route != nil {
			validUntil = r.Route.ValidUntil.UTC
		}
		if !validUntil.IsZero() {
			untilNanos = min(untilNanos, facts.NowNanos+uint64(max(0, validUntil.Sub(facts.WallTime))))
			if !facts.WallTime.Before(validUntil) {
				gate = False
				state.Reason, state.Dependency = "lifetime_expired", ""
			}
		}
		if gate == True && r.EgressRoute != nil {
			route, until, available := r.EgressRoute.selectRoute(facts)
			if available {
				node.egress = &route
				untilNanos = min(untilNanos, until)
			} else {
				gate, state.Reason = False, "no_ready_egress"
			}
		}
		state.Active = gate == True
		state.Ready = gate
		readyUntilNanos := untilNanos
		if state.Active {
			if r.ReadyWhen != nil {
				var until uint64
				state.Ready, until = facts.ConditionAuthority(*r.ReadyWhen)
				readyUntilNanos = min(readyUntilNanos, until)
			}
			if state.Ready != True {
				state.Reason = conditionReason("ready_when", state.Ready)
				if facts.pendingHold(r.ReadyWhen) {
					state.Reason = "ready_when_hold"
				}
			}
			if r.EgressPolicy != nil && !r.EgressPolicy.available(facts) {
				state.Ready, state.Reason = False, "no_ready_egress"
			}
			selected = append(selected, node)
		}
		if state.Active && untilNanos != math.MaxUint64 {
			state.ActiveUntil = lifetime.Deadline{BootID: facts.Scope.BootID, Nanos: untilNanos}
		}
		states[r.ID] = state
		bounds[r.ID] = readyUntilNanos
	}
	native := renderNative(selected)
	interfaces := make(map[string]*network.Interface, len(native.Network.Interfaces))
	for i := range native.Network.Interfaces {
		iface := &native.Network.Interfaces[i]
		interfaces[iface.Name] = iface
	}
	clock := boottime.Clock{BootID: facts.Scope.BootID, BoottimeNanoseconds: int64(facts.NowNanos), WallTime: facts.WallTime}
	linkIndex, routeIndex, ruleIndex, natIndex := 0, 0, 0, 0
	for _, node := range selected {
		authority := states[node.source.ID].ActiveUntil
		switch {
		case node.source.Link != nil:
			if authority != (lifetime.Deadline{}) {
				native.Network.Links[linkIndex].Authority = authority
			}
			linkIndex++
		case node.source.Route != nil || node.source.EgressRoute != nil:
			if authority != (lifetime.Deadline{}) {
				route := &native.Network.Routes[routeIndex]
				route.BootID, route.ExpiresNanos = authority.BootID, authority.Nanos
			}
			routeIndex++
		case node.source.PolicyRule != nil:
			native.Network.PolicyRules[ruleIndex].Authority = authority
			ruleIndex++
		case node.source.NAT != nil:
			for range node.source.NAT {
				native.Network.Firewall.NAT[natIndex].Authority = authority
				natIndex++
			}
		case node.source.Address != nil && authority != (lifetime.Deadline{}):
			address := node.source.Address
			iface := interfaces[address.Interface]
			if iface.AddressOptions == nil {
				iface.AddressOptions = make(map[string]network.AddressOptions)
			}
			iface.AddressOptions[address.CIDR] = network.BoundAddressLifetime(iface.AddressOptions[address.CIDR], clock, authority.Nanos)
		}
	}
	return native, states
}

func conditionReason(prefix string, value Truth) string {
	if value == False {
		return prefix + "_false"
	}
	return prefix + "_indeterminate"
}

// renderNative assembles selected, already compiled resources. Only output
// collections are mutable; configuration payloads remain owned by their plan.
func renderNative(nodes []compiledResource) Native {
	var out Native
	var nat []network.NATRule
	interfaces := make(map[string]int)
	ensureInterface := func(name string) int {
		if index, exists := interfaces[name]; exists {
			return index
		}
		index := len(out.Network.Interfaces)
		interfaces[name] = index
		out.Network.Interfaces = append(out.Network.Interfaces, network.Interface{Name: name})
		return index
	}
	for _, node := range nodes {
		r := node.source
		switch {
		case r.Interface != nil:
			index := ensureInterface(r.Interface.Name)
			previous := out.Network.Interfaces[index]
			out.Network.Interfaces[index] = *r.Interface
			out.Network.Interfaces[index].Addresses = previous.Addresses
			out.Network.Interfaces[index].AddressOptions = previous.AddressOptions
		case r.Address != nil:
			index := ensureInterface(r.Address.Interface)
			iface := &out.Network.Interfaces[index]
			iface.Addresses = append(iface.Addresses, r.Address.CIDR)
			if r.Address.Options != nil {
				if iface.AddressOptions == nil {
					iface.AddressOptions = make(map[string]network.AddressOptions)
				}
				iface.AddressOptions[r.Address.CIDR] = *r.Address.Options
			}
		case r.Link != nil:
			out.Network.Links = append(out.Network.Links, *r.Link)
		case r.Route != nil:
			out.Network.Routes = append(out.Network.Routes, *r.Route)
		case r.EgressRoute != nil:
			out.Network.Routes = append(out.Network.Routes, *node.egress)
		case r.PolicyRule != nil:
			out.Network.PolicyRules = append(out.Network.PolicyRules, *r.PolicyRule)
		case r.Forwarding != nil:
			if r.Forwarding.ConntrackTCPLiberal != nil {
				out.Network.Forwarding.ConntrackTCPLiberal = r.Forwarding.ConntrackTCPLiberal
			}
			if r.Forwarding.IPv4 != nil {
				out.Network.Forwarding.IPv4 = r.Forwarding.IPv4
			}
			if r.Forwarding.IPv6 != nil {
				out.Network.Forwarding.IPv6 = r.Forwarding.IPv6
			}
			if r.Forwarding.IPv4RPFilter != nil {
				out.Network.Forwarding.IPv4RPFilter = r.Forwarding.IPv4RPFilter
			}
			if r.Forwarding.IPv4ECMPHash != "" {
				out.Network.Forwarding.IPv4ECMPHash = r.Forwarding.IPv4ECMPHash
			}
		case r.Firewall != nil:
			out.Network.Firewall = r.Firewall
		case r.NAT != nil:
			nat = append(nat, r.NAT...)
		case r.VRRPGuard != nil:
			out.Network.VRRPGuards = append(out.Network.VRRPGuards, *r.VRRPGuard)
		case r.WireGuard != nil:
			out.Network.WireGuard = append(out.Network.WireGuard, *r.WireGuard)
		case r.Service != nil:
			out.Services = append(out.Services, *r.Service)
		case r.EventRule != nil:
			out.EventRules = append(out.EventRules, node.rule)
		case r.DerivedEvent != nil:
			out.DerivedEvents = append(out.DerivedEvents, node.derived)
		}
	}
	if len(nat) > 0 {
		firewall := network.Firewall{DisableForward: true}
		if out.Network.Firewall != nil {
			firewall = *out.Network.Firewall
		}
		firewall.NAT = nat
		out.Network.Firewall = &firewall
	}
	return out
}
