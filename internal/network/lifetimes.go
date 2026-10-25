package network

import "github.com/imksoo/routerd2/internal/boottime"

// ProjectLifetimes removes expired addresses and routes at one paired clock.
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
		if (route.ValidUntil.IsZero() || route.ValidUntil.After(clock.WallTime)) && (route.BootID == "" || route.BootID == clock.BootID && route.ExpiresNanos > uint64(clock.BoottimeNanoseconds)) {
			kept = append(kept, route)
		}
	}
	d.Routes = kept
	return d
}
