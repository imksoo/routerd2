// SPDX-License-Identifier: BSD-3-Clause
// Selection and rendezvous identities follow routerd/pkg/egressroute and
// pkg/render/nftables.go at 1329ef54f296. Runtime observations stay typed here.
package resourceprogram

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

type EgressPolicy struct {
	MarkOnly            bool          `json:"mark_only,omitempty"`
	Sources             []string      `json:"sources,omitempty"`
	Destinations        []string      `json:"destinations,omitempty"`
	ExcludeDestinations []string      `json:"exclude_destinations,omitempty"`
	InInterface         string        `json:"in_interface,omitempty"`
	HashDestination     bool          `json:"hash_destination,omitempty"`
	HysteresisMillis    int           `json:"hysteresis_millis,omitempty"`
	Groups              []EgressGroup `json:"groups"`
}

type EgressGroup struct {
	Name     string         `json:"name"`
	Weight   int            `json:"weight"`
	Priority int            `json:"priority,omitempty"`
	When     *FactCondition `json:"when,omitempty"`
	Targets  []EgressTarget `json:"targets"`
}

type EgressTarget struct {
	Name      string         `json:"name"`
	Interface string         `json:"interface"`
	Gateway   string         `json:"gateway,omitempty"`
	Table     int            `json:"table"`
	Priority  int            `json:"priority"`
	Mark      uint32         `json:"mark"`
	Metric    int            `json:"metric,omitempty"`
	When      *FactCondition `json:"when,omitempty"`
	// OwnerWhen bounds the underlying path. Health controls new selection;
	// an existing marked connection retains its old path while it is still owned.
	OwnerWhen *FactCondition `json:"owner_when,omitempty"`
}

type EgressSelection struct {
	group, bootID string
	sinceNanos    uint64
}

type EgressPolicyPlan struct {
	ID string
	// Fallbacks, Rules and Marks are configuration-owned. They remain when
	// observations withdraw a live path, so marked traffic cannot use main.
	Fallbacks  []network.Route
	Rules      []network.PolicyRule
	Marks      []uint32
	Flows      []network.FlowRoute
	spec       EgressPolicy
	seed       uint32
	scores     [][][]uint64
	identities [][]string
}

type EgressPolicyProjection struct {
	Selection EgressSelection
	Routes    []network.Route
	Flows     []network.FlowRoute
}

func prepareEgressPolicy(id string, spec EgressPolicy) (EgressPolicyPlan, error) {
	p := EgressPolicyPlan{ID: id, spec: spec}
	if len(spec.Groups) == 0 || len(spec.Groups) > 64 || spec.HysteresisMillis < 0 || uint64(spec.HysteresisMillis) > uint64(math.MaxInt64)/uint64(time.Millisecond) {
		return p, errors.New("egress policy requires 1..64 groups and nonnegative hysteresis")
	}
	for _, values := range [][]string{spec.Sources, spec.Destinations, spec.ExcludeDestinations} {
		for _, value := range values {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || !prefix.Addr().Is4() {
				return p, errors.New("egress policy selectors require IPv4 prefixes")
			}
		}
	}
	if len(p.spec.Sources) == 0 {
		p.spec.Sources = []string{"0.0.0.0/0"}
	}
	if len(p.spec.Destinations) == 0 {
		p.spec.Destinations = []string{"0.0.0.0/0"}
	}
	digest := sha256.Sum256([]byte(id))
	p.seed = binary.BigEndian.Uint32(digest[:4])
	groups, marks, tables, priorities := map[string]bool{}, map[uint32]bool{}, map[int]bool{}, map[int]bool{}
	for gi, g := range spec.Groups {
		if g.Name == "" || groups[g.Name] || len(g.Targets) == 0 || len(g.Targets) > 64 {
			return p, errors.New("egress groups require distinct names and 1..64 targets")
		}
		groups[g.Name] = true
		if g.When != nil {
			if err := ValidateFactCondition(*g.When); err != nil {
				return p, err
			}
		}
		p.scores = append(p.scores, make([][]uint64, len(g.Targets)))
		p.identities = append(p.identities, make([]string, len(g.Targets)))
		for ti, target := range g.Targets {
			if target.Name == "" || target.Interface == "" || target.Mark == 0 || target.Table <= 0 || target.Priority < 1 || target.Priority > 32765 || marks[target.Mark] || tables[target.Table] || priorities[target.Priority] {
				return p, errors.New("egress targets require distinct marks, tables and policy priorities")
			}
			marks[target.Mark], tables[target.Table], priorities[target.Priority] = true, true, true
			for _, condition := range []*FactCondition{target.When, target.OwnerWhen} {
				if condition != nil {
					if err := ValidateFactCondition(*condition); err != nil {
						return p, err
					}
				}
			}
			if target.Gateway != "" {
				ip, err := netip.ParseAddr(target.Gateway)
				if err != nil || !ip.Is4() {
					return p, errors.New("IPv4 egress gateway must be an IPv4 address")
				}
			}
			identity := strings.Join([]string{target.Name, target.Interface, fmt.Sprintf("mark-%08x", target.Mark), strconv.Itoa(target.Table)}, "\x1f")
			p.identities[gi][ti] = identity
			p.scores[gi][ti] = make([]uint64, 256)
			for bucket := range p.scores[gi][ti] {
				key := id + "\x00" + g.Name + "\x00" + identity + "\x00" + strconv.Itoa(bucket)
				sum := sha256.Sum256([]byte(key))
				p.scores[gi][ti][bucket] = binary.BigEndian.Uint64(sum[:8])
			}
		}
	}
	var validation network.Desired
	validation.Firewall = &network.Firewall{}
	for _, group := range p.spec.Groups {
		for _, target := range group.Targets {
			if uint64(target.Metric) >= math.MaxUint32 {
				return p, errors.New("egress target metric leaves no lower-priority metric for its blackhole fallback")
			}
			mark := target.Mark
			p.Fallbacks = append(p.Fallbacks, network.Route{Destination: "0.0.0.0/0", Table: target.Table, Metric: target.Metric + 1, Blackhole: true})
			p.Rules = append(p.Rules, network.PolicyRule{Family: 4, Table: target.Table, Priority: target.Priority, Mark: &mark})
			p.Marks = append(p.Marks, target.Mark)
			validation.Routes = append(validation.Routes, network.Route{Destination: "0.0.0.0/0", Interface: target.Interface, Gateway: target.Gateway, Table: target.Table, Metric: target.Metric})
		}
	}
	validation.Routes = append(validation.Routes, p.Fallbacks...)
	validation.PolicyRules = p.Rules
	validation.Firewall.FlowMarks = p.Marks
	if !p.spec.MarkOnly {
		for _, source := range p.spec.Sources {
			for _, destination := range p.spec.Destinations {
				p.Flows = append(p.Flows, network.FlowRoute{Match: network.PacketMatch{Family: 4, Source: source, Destination: destination, InInterface: p.spec.InInterface}, ExcludeDestinations: p.spec.ExcludeDestinations, Seed: p.seed, HashDestination: p.spec.HashDestination})
			}
		}
	}
	validation.Firewall.FlowRoutes = p.Flows
	if err := network.Validate(validation); err != nil {
		return p, err
	}
	return p, nil
}

func policyCondition(facts FactSnapshot, condition *FactCondition) (bool, uint64) {
	if condition == nil {
		return true, math.MaxUint64
	}
	value, until := facts.ConditionAuthority(*condition)
	return value == True, until
}

func (p EgressPolicy) available(facts FactSnapshot) bool {
	if p.MarkOnly {
		return true
	}
	for _, group := range p.Groups {
		active, _ := policyCondition(facts, group.When)
		if !active {
			continue
		}
		for _, target := range group.Targets {
			ready, _ := policyCondition(facts, target.When)
			owned, _ := policyCondition(facts, target.OwnerWhen)
			if ready && owned {
				return true
			}
		}
	}
	return false
}

// Project preserves routerd's weight/priority/name/order choice and only holds
// a previous selection while it remains ready. Failed paths never wait out the
// recovery hysteresis. Prepared scores avoid hashing static identities on ticks.
func (p EgressPolicyPlan) Project(facts FactSnapshot, previous EgressSelection) EgressPolicyProjection {
	out := EgressPolicyProjection{Flows: slices.Clone(p.Flows)}
	ready := make([][]int, len(p.spec.Groups))
	bounds := make([]uint64, len(p.spec.Groups))
	selected, old := -1, -1
	for gi, g := range p.spec.Groups {
		active, until := policyCondition(facts, g.When)
		bounds[gi] = until
		for ti, target := range g.Targets {
			owned, ownerUntil := policyCondition(facts, target.OwnerWhen)
			if owned {
				deadline := lifetime.Deadline{BootID: facts.Scope.BootID, Nanos: ownerUntil}
				if ownerUntil == math.MaxUint64 {
					deadline = lifetime.Deadline{}
				}
				out.Routes = append(out.Routes, network.Route{Destination: "0.0.0.0/0", Interface: target.Interface, Gateway: target.Gateway, Table: target.Table, Metric: target.Metric, BootID: deadline.BootID, ExpiresNanos: deadline.Nanos})
			}
			healthy, healthUntil := policyCondition(facts, target.When)
			if active && owned && healthy {
				ready[gi] = append(ready[gi], ti)
				bounds[gi] = min(bounds[gi], ownerUntil, healthUntil)
			}
		}
		if len(ready[gi]) == 0 {
			continue
		}
		if g.Name == previous.group {
			old = gi
		}
		if selected < 0 || g.Weight > p.spec.Groups[selected].Weight || g.Weight == p.spec.Groups[selected].Weight && (g.Priority < p.spec.Groups[selected].Priority || g.Priority == p.spec.Groups[selected].Priority && g.Name < p.spec.Groups[selected].Name) {
			selected = gi
		}
	}
	if p.spec.MarkOnly {
		return out
	}
	if selected < 0 {
		return out
	}
	if old >= 0 && old != selected && previous.bootID == facts.Scope.BootID && facts.NowNanos >= previous.sinceNanos && facts.NowNanos-previous.sinceNanos < uint64(time.Duration(p.spec.HysteresisMillis)*time.Millisecond) {
		selected = old
	}
	group := p.spec.Groups[selected]
	out.Selection = previous
	if previous.group != group.Name || previous.bootID != facts.Scope.BootID {
		out.Selection = EgressSelection{group: group.Name, bootID: facts.Scope.BootID, sinceNanos: facts.NowNanos}
	}
	buckets := []uint32{group.Targets[ready[selected][0]].Mark}
	if len(ready[selected]) > 1 {
		buckets = make([]uint32, 256)
	}
	for bucket := range buckets {
		best := ready[selected][0]
		for _, ti := range ready[selected][1:] {
			score, previousScore := p.scores[selected][ti][bucket], p.scores[selected][best][bucket]
			if score > previousScore || score == previousScore && p.identities[selected][ti] < p.identities[selected][best] {
				best = ti
			}
		}
		buckets[bucket] = group.Targets[best].Mark
	}
	authority := lifetime.Deadline{BootID: facts.Scope.BootID, Nanos: bounds[selected]}
	if bounds[selected] == math.MaxUint64 {
		authority = lifetime.Deadline{}
	}
	for i := range out.Flows {
		out.Flows[i].Buckets = buckets
		out.Flows[i].Authority = authority
	}
	return out
}
