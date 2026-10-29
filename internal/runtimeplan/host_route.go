package runtimeplan

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// The existing coordinator observes DAD on addresses in its acknowledged
// projection. These are kernel observations, not a second address allocator.
// The network RPC runs outside mu; cutover discards the old source scope.
func (r *Runtime) observeHostSources(ctx context.Context) error {
	r.mu.Lock()
	epoch := r.sourceEpoch
	if len(r.config.plan.HostRoutes) == 0 {
		r.hostSources = nil
		r.mu.Unlock()
		return nil
	}
	candidates := make(map[string][]string)
	if r.applied.network != nil {
		for _, host := range r.config.plan.HostRoutes {
			if _, seen := candidates[host.Route.Interface]; seen {
				continue
			}
			candidates[host.Route.Interface] = nil
			for _, iface := range r.applied.network.Interfaces {
				if iface.Name != host.Route.Interface {
					continue
				}
				for _, cidr := range iface.Addresses {
					prefix, _ := netip.ParsePrefix(cidr)
					if prefix.IsValid() && prefix.Addr().Is6() && prefix.Addr().IsGlobalUnicast() {
						candidates[iface.Name] = append(candidates[iface.Name], prefix.Addr().String())
					}
				}
			}
		}
	}
	r.mu.Unlock()
	observed := make(map[string]network.SourceObservation, len(candidates))
	var observeErr error
	for iface, addresses := range candidates {
		for _, address := range addresses {
			source, err := r.network.SourceAddress(ctx, iface, address)
			if err != nil {
				observeErr = fmt.Errorf("observe IPv6 host source on %s: %w", iface, err)
				break
			}
			if sourceInstalled(iface, address, source, false) {
				observed[iface] = source
				break
			}
		}
	}
	r.mu.Lock()
	if epoch == r.sourceEpoch {
		changed := len(r.hostSources) != len(observed)
		for iface, source := range observed {
			previous := r.hostSources[iface]
			changed = changed || previous.Index != source.Index || previous.RequestedAddress != source.RequestedAddress
		}
		if changed {
			r.observationVersion++
		}
		r.hostSources = observed
	}
	r.mu.Unlock()
	return observeErr
}

func hostSourceDeadline(options network.AddressOptions, clock boottime.Clock) (lifetime.Deadline, bool) {
	if b := options.BootLifetime; b.BootID != "" {
		until := min(b.PreferredNanos, b.ValidNanos)
		if b.BootID != clock.BootID || until <= uint64(clock.BoottimeNanoseconds) {
			return lifetime.Deadline{}, false
		}
		if until == network.InfiniteNanos {
			return lifetime.Deadline{}, true
		}
		return lifetime.Deadline{BootID: b.BootID, Nanos: until}, true
	}
	until := lifetime.Minimum(clock, lifetime.Deadline{UTC: options.PreferredUntil}, lifetime.Deadline{UTC: options.ValidUntil})
	return until, until.Live(clock)
}

func (r *Runtime) projectHostRoutesLocked(cfg configuration, facts resourceprogram.FactSnapshot, p *projection) {
	clock := boottime.Clock{BootID: facts.Scope.BootID, BoottimeNanoseconds: int64(facts.NowNanos), WallTime: facts.WallTime}
	for _, host := range cfg.plan.HostRoutes {
		state := p.resources[host.ID]
		if !state.Active {
			continue
		}
		ready, reason := state.Ready, state.Reason
		source := r.hostSources[host.Route.Interface]
		state.Ready, state.Reason = resourceprogram.False, "preferred_source_unavailable"
		p.resources[host.ID] = state
		if !sourceInstalled(host.Route.Interface, source.RequestedAddress, source, false) {
			continue
		}
		var bound lifetime.Deadline
		found := false
		for _, iface := range p.network.Interfaces {
			if iface.Name != host.Route.Interface {
				continue
			}
			for _, cidr := range iface.Addresses {
				prefix, _ := netip.ParsePrefix(cidr)
				if prefix.Addr().String() != source.RequestedAddress {
					continue
				}
				bound, found = hostSourceDeadline(iface.AddressOptions[cidr], clock)
				break
			}
		}
		if !found {
			continue
		}
		bound = lifetime.Minimum(clock, bound, state.ActiveUntil)
		route := host.Route
		if host.RouterFrom != "" {
			preference := -2
			var routerUntil lifetime.Deadline
			for _, learned := range p.routers[host.RouterFrom] {
				if learned.Destination == "::/0" && learned.ValidUntil.Live(clock) && (learned.Preference > preference || learned.Preference == preference && learned.Gateway < route.Gateway) {
					route.Gateway, preference, routerUntil = learned.Gateway, learned.Preference, learned.ValidUntil
				}
			}
			if route.Gateway == "" {
				state.Reason = "router_unavailable"
				p.resources[host.ID] = state
				continue
			}
			bound = lifetime.Minimum(clock, bound, routerUntil)
		}
		route.PreferredSource, route.ValidUntil = source.RequestedAddress, bound
		p.network.Routes = append(p.network.Routes, route)
		rule := host.Rule
		if bound.BootID == "" && !bound.UTC.IsZero() {
			rule.Authority = lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds) + uint64(bound.Remaining(clock))}
		} else {
			rule.Authority = bound
		}
		localRule := host.LocalRule
		localRule.Authority = rule.Authority
		p.network.PolicyRules = append(p.network.PolicyRules, localRule, rule)
		state.Ready, state.Reason = ready, reason
		p.resources[host.ID] = state
		for _, bypass := range host.Bypasses {
			input, ok := r.aftrInputs[bypass.Service]
			binding := r.bindings[bypass.Service]
			if !ok || binding.digest != input.ConfigDigest || input.Source.Address == "" || !input.Source.PreferredUntil.Live(clock) || !input.Source.ValidUntil.Live(clock) {
				continue
			}
			until := lifetime.Minimum(clock, input.Source.PreferredUntil, input.Source.ValidUntil, state.ActiveUntil)
			if until.BootID == "" && !until.UTC.IsZero() {
				until = lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds) + uint64(until.Remaining(clock))}
			}
			p.network.PolicyRules = append(p.network.PolicyRules, network.PolicyRule{Family: 6, Priority: bypass.Priority, Table: 254, InInterface: "lo", Source: input.Source.Address + "/128", Authority: until})
		}
	}
}
