package runtimeplan

import (
	"maps"
	"reflect"
	"slices"

	"github.com/imksoo/routerd2/internal/network"
)

// A newer observation can extend an existing kernel grant without invalidating
// its acknowledged effects. Compare against the original grant; this neither
// acknowledges the extension nor changes the writer's expiry. The caller still
// checks the acknowledged deadline before reporting ready.
func networkCovered(applied, desired network.Desired) bool {
	if len(applied.Links) != len(desired.Links) || len(applied.Interfaces) != len(desired.Interfaces) || len(applied.Routes) != len(desired.Routes) || len(applied.PolicyRules) != len(desired.PolicyRules) {
		return false
	}
	desired.Links = slices.Clone(desired.Links)
	for i, old := range applied.Links {
		current := &desired.Links[i]
		if old.Authority.BootID != "" && current.Authority.BootID == old.Authority.BootID && current.Authority.Nanos >= old.Authority.Nanos {
			current.Authority = old.Authority
		}
	}
	desired.Interfaces = slices.Clone(desired.Interfaces)
	for i, old := range applied.Interfaces {
		current := &desired.Interfaces[i]
		current.AddressOptions = maps.Clone(current.AddressOptions)
		for address, options := range current.AddressOptions {
			previous, ok := old.AddressOptions[address]
			if !ok {
				continue
			}
			before, after := previous.BootLifetime, options.BootLifetime
			if before.BootID != "" && after.BootID == before.BootID && after.PreferredNanos >= before.PreferredNanos && after.ValidNanos >= before.ValidNanos {
				options.BootLifetime = before
				current.AddressOptions[address] = options
			}
		}
	}
	desired.Routes = slices.Clone(desired.Routes)
	for i, old := range applied.Routes {
		current := &desired.Routes[i]
		if old.BootID != "" && current.BootID == old.BootID && current.ExpiresNanos >= old.ExpiresNanos {
			current.ExpiresNanos = old.ExpiresNanos
		}
		if old.ValidUntil.BootID != "" && current.ValidUntil.BootID == old.ValidUntil.BootID && current.ValidUntil.Nanos >= old.ValidUntil.Nanos {
			current.ValidUntil = old.ValidUntil
		}
	}
	desired.PolicyRules = slices.Clone(desired.PolicyRules)
	for i, old := range applied.PolicyRules {
		current := &desired.PolicyRules[i]
		if old.Authority.BootID != "" && current.Authority.BootID == old.Authority.BootID && current.Authority.Nanos >= old.Authority.Nanos {
			current.Authority = old.Authority
		}
	}
	if applied.Firewall != nil && desired.Firewall != nil && len(applied.Firewall.NAT) == len(desired.Firewall.NAT) {
		firewall := *desired.Firewall
		firewall.NAT = slices.Clone(firewall.NAT)
		for i, old := range applied.Firewall.NAT {
			current := &firewall.NAT[i]
			if old.Authority.BootID != "" && current.Authority.BootID == old.Authority.BootID && current.Authority.Nanos >= old.Authority.Nanos {
				current.Authority = old.Authority
			}
		}
		desired.Firewall = &firewall
	}
	return reflect.DeepEqual(applied, desired)
}
