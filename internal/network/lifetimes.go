package network

import (
	"slices"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// ProjectLifetimes removes expired addresses, routes and rules at one paired clock.
// The caller owns the slices and address-option maps: projection mutates these
// collections. Control uses it before computing the expected application;
// the writer rechecks after transport and other intervening work.
func ProjectLifetimes(d Desired, clock boottime.Clock) Desired {
	for i := range d.Interfaces {
		iface := &d.Interfaces[i]
		kept := iface.Addresses[:0]
		for _, address := range iface.Addresses {
			options := iface.AddressOptions[address]
			lifetime := options.BootLifetime
			valid := (lifetime.BootID == "" || lifetime.BootID == clock.BootID && lifetime.ValidNanos > uint64(clock.BoottimeNanoseconds)) && (options.ValidUntil.IsZero() || options.ValidUntil.After(clock.WallTime))
			if !valid {
				delete(iface.AddressOptions, address)
				continue
			}
			kept = append(kept, address)
		}
		iface.Addresses = kept
		if len(iface.AddressOptions) == 0 {
			iface.AddressOptions = nil
		}
	}
	kept := d.Routes[:0]
	for _, route := range d.Routes {
		if route.ValidUntil.Live(clock) && (route.BootID == "" || route.BootID == clock.BootID && route.ExpiresNanos > uint64(clock.BoottimeNanoseconds)) {
			kept = append(kept, route)
		}
	}
	d.Routes = kept
	rules := d.PolicyRules[:0]
	for _, rule := range d.PolicyRules {
		if rule.Authority.Live(clock) {
			rules = append(rules, rule)
		}
	}
	d.PolicyRules = rules
	if d.Firewall != nil {
		f := *d.Firewall
		f.NAT = slices.DeleteFunc(slices.Clone(f.NAT), func(rule NATRule) bool { return !rule.Authority.Live(clock) })
		f.FlowRoutes = slices.Clone(f.FlowRoutes)
		for i := range f.FlowRoutes {
			if !f.FlowRoutes[i].Authority.Live(clock) {
				f.FlowRoutes[i].Buckets = nil
				f.FlowRoutes[i].Authority = lifetime.Deadline{}
			}
		}
		d.Firewall = &f
	}
	return d
}
