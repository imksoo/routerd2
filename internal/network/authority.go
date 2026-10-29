package network

import (
	"context"
	"errors"
	"net"
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
)

// MaintainAuthority runs in the network worker, independently of control requests.
// It enforces original protocol deadlines for routes, addresses and virtual
// links, including after a writer restart.
// A paused or unavailable writer cannot enforce IPv4 expiry; this is not a
// kernel timer. The worker owner must cancel and join this loop before Close.
func (e *Engine) MaintainAuthority(ctx context.Context) error {
	for {
		delay, err := e.expireAuthority(ctx)
		if err != nil {
			return err
		}
		var wake <-chan time.Time
		var timer *time.Timer
		if delay > 0 {
			// Go timers exclude suspend. Re-read BOOTTIME within a second of
			// resuming, even if the original deadline passed while suspended.
			timer = time.NewTimer(min(delay, time.Second))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
		case <-e.authorityWake:
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (e *Engine) expireAuthority(ctx context.Context) (time.Duration, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lock == nil {
		return 0, errors.New("network engine is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	started := time.Now()
	clock, err := readRouteClock()
	if err != nil {
		return 0, err
	}
	var delay time.Duration
	p := &apply{e: e, ctx: ctx}
	before := len(e.state.Routes)
	var expiredLinks map[string]bool
	for i, owned := range e.state.Links {
		link := owned.Definition
		if link.Authority == (lifetime.Deadline{}) {
			continue
		}
		if link.Authority.Live(clock.Clock) {
			remaining := link.Authority.Remaining(clock.Clock)
			if delay == 0 || remaining < delay {
				delay = remaining
			}
			if link.HardwareAddr != "" {
				if owned.announceAt <= uint64(clock.BoottimeNanoseconds) {
					live, err := e.kernel.Interface(link.Name)
					if err == nil {
						err = verifyOwnedLink(live, owned)
					}
					if err != nil {
						return 0, err
					}
					if live.Up {
						mac, err := net.ParseMAC(link.HardwareAddr)
						if err != nil {
							return 0, err
						}
						if err := e.kernel.AnnounceLink(linkAnnouncement{Index: owned.Index, MAC: mac, Authority: link.Authority}); err != nil {
							return 0, err
						}
					}
					e.state.Links[i].announceAt = uint64(clock.BoottimeNanoseconds) + uint64(time.Second)
				}
				remaining = time.Duration(e.state.Links[i].announceAt - uint64(clock.BoottimeNanoseconds))
				if delay == 0 || remaining < delay {
					delay = remaining
				}
			}

			continue
		}
		e.state.Links[i].announceAt = 0
		live, err := e.kernel.Interface(link.Name)
		if errors.Is(err, errLinkNotFound) {
			if expiredLinks == nil {
				expiredLinks = make(map[string]bool)
			}
			expiredLinks[link.Name] = true
			continue
		}
		if err == nil {
			err = verifyOwnedLink(live, owned)
		}
		if err == nil {
			err = p.set(settingKey{Kind: "up", Interface: link.Name}, 0)
		}
		if err != nil {
			e.state.Status, e.state.LastError = "degraded", err.Error()
			return 0, errors.Join(err, e.save())
		}
		if expiredLinks == nil {
			expiredLinks = make(map[string]bool)
		}
		expiredLinks[link.Name] = true
	}
	for i := 0; i < len(e.state.Routes); {
		r := e.state.Routes[i]
		expired := expiredLinks[r.Interface] || !r.ValidUntil.Live(clock.Clock)
		if r.BootID != "" {
			if r.BootID != clock.BootID || r.ExpiresNanos <= uint64(clock.BoottimeNanoseconds) {
				expired = true
			} else {
				remaining := time.Duration(r.ExpiresNanos - uint64(clock.BoottimeNanoseconds))
				if delay == 0 || remaining < delay {
					delay = remaining
				}
			}
		}
		for _, hop := range r.NextHops {
			expired = expired || expiredLinks[hop.Interface]
		}
		if !expired {
			if timer := r.KernelExpiry; timer.BootID == clock.BootID {
				renewIn := time.Duration(int64(timer.Nanos)-clock.monotonicNanos) - routeKernelRenewBefore
				if r.ValidUntil.Remaining(clock.Clock) > time.Duration(int64(timer.Nanos)-clock.monotonicNanos)+2*time.Second && renewIn <= 0 {
					if err := p.refreshRoute(i, r.Route); err != nil {
						return 0, err
					}
					i++
					continue
				}
				if renewIn > 0 && (delay == 0 || renewIn < delay) {
					delay = renewIn
				}
			}
			if remaining := r.ValidUntil.Remaining(clock.Clock); r.ValidUntil != (lifetime.Deadline{}) && (delay == 0 || remaining < delay) {
				delay = remaining
			}
			i++
			continue
		}
		if err := p.removeRoute(i); err != nil {
			e.state.Status, e.state.LastError = "degraded", err.Error()
			return 0, errors.Join(err, e.save())
		}
	}
	for i := 0; i < len(e.state.PolicyRules); {
		authority := e.state.PolicyRules[i].Authority
		if authority == (lifetime.Deadline{}) {
			i++
			continue
		}
		if authority.Live(clock.Clock) {
			if remaining := authority.Remaining(clock.Clock); delay == 0 || remaining < delay {
				delay = remaining
			}
			i++
			continue
		}
		if err := p.removePolicyRule(i); err != nil {
			e.state.Status, e.state.LastError = "degraded", err.Error()
			return 0, errors.Join(err, e.save())
		}
	}
	if firewall := e.state.Desired.Firewall; firewall != nil {
		changed := false
		next := *firewall
		next.FlowRoutes = slices.Clone(firewall.FlowRoutes)
		for i, flow := range next.FlowRoutes {
			if flow.Authority == (lifetime.Deadline{}) {
				continue
			}
			if flow.Authority.Live(clock.Clock) {
				if remaining := flow.Authority.Remaining(clock.Clock); delay == 0 || remaining < delay {
					delay = remaining
				}
				continue
			}
			next.FlowRoutes[i].Buckets, next.FlowRoutes[i].Authority = nil, lifetime.Deadline{}
			changed = true
		}
		if changed {
			if err := p.firewall(&next); err != nil {
				return 0, err
			}
			e.state.Desired.Firewall = &next
			if err := e.save(); err != nil {
				return 0, err
			}
		}
	}
	keep := make(map[addressKey]bool, len(e.state.Addresses))
	expiredAddresses := false
	for _, address := range e.state.Addresses {
		if expiredLinks[address.Interface] {
			expiredAddresses = true
			continue
		}
		if lifetime := address.Options.BootLifetime; lifetime.BootID != "" {
			if lifetime.BootID != clock.BootID || lifetime.ValidNanos <= uint64(clock.BoottimeNanoseconds) {
				expiredAddresses = true
				continue
			}
			if lifetime.ValidNanos != InfiniteNanos {
				remaining := time.Duration(lifetime.ValidNanos - uint64(clock.BoottimeNanoseconds))
				if delay == 0 || remaining < delay {
					delay = remaining
				}
			}
		}
		keep[address.key()] = true
	}
	if expiredAddresses {
		if err := p.removeAddresses(keep); err != nil {
			e.state.Status, e.state.LastError = "degraded", err.Error()
			return 0, errors.Join(err, e.save())
		}
	}
	if len(e.state.Routes) != before {
		if err := e.save(); err != nil {
			return 0, err
		}
	}
	if delay > 0 {
		delay = max(time.Nanosecond, delay-time.Since(started))
	}
	return delay, nil
}

// Both configuration removal and autonomous expiry use the same ownership
// check and durable removal. A changed or replaced foreign route is left alone.
func (p *apply) removeRoute(i int) error {
	r := p.e.state.Routes[i]
	live, err := p.routeTable(routeTable(r.Route))
	if err != nil {
		return p.failure("route remove read", err)
	}
	forget := func() {
		// Ownership is unordered. Compact in constant time when a whole
		// learned FIB expires, rather than shifting it after every removal.
		last := len(p.e.state.Routes) - 1
		p.e.state.Routes[i] = p.e.state.Routes[last]
		p.e.state.Routes[last] = ownedRoute{}
		p.e.state.Routes = p.e.state.Routes[:last]
	}
	key := routeSlot(r.Route)
	for j, v := range live[key] {
		if routeMatches(v, r) {
			if err := p.ctx.Err(); err != nil {
				return p.failure("route remove", err)
			}
			if err := p.e.kernel.DeleteRoute(r.Route); err != nil {
				return p.failure("route remove", err)
			}
			p.changed++
			forget()
			live[key] = append(live[key][:j], live[key][j+1:]...)
			return nil
		}
	}
	forget()
	return nil
}

// One kernel dump per table and operation avoids a complete FIB walk for each
// renewed or expired route. Removals update this operation's observation.
func (p *apply) routeTable(table int) (map[string][]RouteState, error) {
	if live, ok := p.routeTables[table]; ok {
		return live, nil
	}
	routes, err := p.e.kernel.Routes(table)
	if err != nil {
		return nil, err
	}
	live := make(map[string][]RouteState, len(routes))
	for _, r := range routes {
		key := routeSlot(r.Route)
		live[key] = append(live[key], r)
	}
	if p.routeTables == nil {
		p.routeTables = make(map[int]map[string][]RouteState)
	}
	p.routeTables[table] = live
	return live, nil
}
