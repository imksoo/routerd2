package runtimeplan

import (
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func (r *Runtime) projectEgressPoliciesLocked(cfg configuration, facts resourceprogram.FactSnapshot, p *projection) {
	p.egressSelections = make(map[string]resourceprogram.EgressSelection, len(cfg.plan.EgressPolicies))
	clock := boottime.Clock{BootID: facts.Scope.BootID, BoottimeNanoseconds: int64(facts.NowNanos), WallTime: facts.WallTime}
	for _, policy := range cfg.plan.EgressPolicies {
		// Routing prohibitions belong to the declared policy, not a temporary
		// path owner. Earlier RPDB rules may still select another valid route.
		p.network.Routes = append(p.network.Routes, policy.Fallbacks...)
		p.network.PolicyRules = append(p.network.PolicyRules, policy.Rules...)
		if p.network.Firewall == nil {
			p.network.Firewall = &network.Firewall{}
		} else {
			f := *p.network.Firewall
			p.network.Firewall = &f
		}
		f := p.network.Firewall
		f.FlowMarks = append(f.FlowMarks[:len(f.FlowMarks):len(f.FlowMarks)], policy.Marks...)
		state := p.resources[policy.ID]
		if !state.Active {
			f.FlowRoutes = append(f.FlowRoutes[:len(f.FlowRoutes):len(f.FlowRoutes)], policy.Flows...)
			continue
		}
		previous := r.egressSelections[policy.ID]
		if cfg.digest != r.config.digest {
			previous = resourceprogram.EgressSelection{}
		}
		selected := policy.Project(facts, previous)
		p.egressSelections[policy.ID] = selected.Selection
		for i := range selected.Routes {
			route := &selected.Routes[i]
			bound := lifetime.Minimum(clock, lifetime.Deadline{BootID: route.BootID, Nanos: route.ExpiresNanos}, state.ActiveUntil)
			route.BootID, route.ExpiresNanos = bound.BootID, bound.Nanos
		}
		for i := range selected.Flows {
			selected.Flows[i].Authority = lifetime.Minimum(clock, selected.Flows[i].Authority, state.ActiveUntil)
		}
		p.network.Routes = append(p.network.Routes, selected.Routes...)
		f.FlowRoutes = append(f.FlowRoutes[:len(f.FlowRoutes):len(f.FlowRoutes)], selected.Flows...)
	}
}
