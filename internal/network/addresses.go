package network

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"
)

// AddressOptions are explicit per-address options. A zero deadline means
// infinite lifetime. A past PreferredUntil deprecates the address while keeping
// it valid; a passed ValidUntil removes it from the projected Desired. Linux
// also expires finite addresses autonomously if the controller is unavailable.
// NoDAD is only for explicitly coordinated IPv6 addresses such as HA VIPs. It
// never changes the DAD policy of another address on the same interface.
type AddressOptions struct {
	PreferredUntil time.Time `json:"preferred_until,omitzero"`
	ValidUntil     time.Time `json:"valid_until,omitzero"`
	NoDAD          bool      `json:"no_dad,omitempty"`
	// Peer is a point-to-point peer address/prefix. The local address must use
	// its host prefix (/32 or /128); the network prefix belongs to the peer.
	// Changing a peer requires removing and applying the local address first.
	Peer string `json:"peer,omitempty"`
}

const addressNoDADFlag = 2

type addressKey struct{ Interface, Address string }

func (a ownedAddress) key() addressKey { return addressKey{a.Interface, a.Address} }

func validateAddressOptions(d Desired) error {
	var errs []error
	for _, l := range d.Interfaces {
		addresses := map[string]bool{}
		for _, s := range l.Addresses {
			if p, err := netip.ParsePrefix(s); err == nil {
				addresses[p.String()] = true
			}
		}
		seen := map[string]bool{}
		for s, o := range l.AddressOptions {
			p, err := netip.ParsePrefix(s)
			if err != nil || !addresses[p.String()] {
				errs = append(errs, fmt.Errorf("interface %s: address_options key %q is not declared in addresses", l.Name, s))
				continue
			}
			if seen[p.String()] {
				errs = append(errs, fmt.Errorf("interface %s: duplicate normalized address_options key %s", l.Name, p))
			}
			seen[p.String()] = true
			if o.NoDAD && !p.Addr().Is6() {
				errs = append(errs, fmt.Errorf("interface %s: NoDAD is supported only for IPv6 address %s", l.Name, p))
			}
			if o.Peer != "" {
				peer, err := netip.ParsePrefix(o.Peer)
				if err != nil || peer.Addr().Is4In6() || peer.Addr().IsUnspecified() || peer.Addr().IsMulticast() || peer.Addr().Is4() != p.Addr().Is4() || peer.Addr() == p.Addr() {
					errs = append(errs, fmt.Errorf("interface %s address %s: peer must be a different same-family unicast address/prefix", l.Name, p))
				}
				if p.Bits() != p.Addr().BitLen() {
					errs = append(errs, fmt.Errorf("interface %s address %s: local point-to-point address must use /32 or /128; prefix length belongs to the peer", l.Name, p))
				}
			}
			if !o.ValidUntil.IsZero() && (o.PreferredUntil.IsZero() || o.PreferredUntil.After(o.ValidUntil)) {
				errs = append(errs, fmt.Errorf("interface %s address %s: preferred lifetime must not exceed valid lifetime", l.Name, p))
			}
			for _, deadline := range []time.Time{o.PreferredUntil, o.ValidUntil} {
				if !deadline.IsZero() && (deadline.Unix() < 0 || deadline.UTC().Year() > 9999) {
					errs = append(errs, fmt.Errorf("interface %s address %s: invalid lifetime deadline", l.Name, p))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func normalizeAddressOptions(o AddressOptions) AddressOptions {
	if p, err := netip.ParsePrefix(o.Peer); err == nil {
		o.Peer = p.String()
	}
	if !o.PreferredUntil.IsZero() {
		o.PreferredUntil = o.PreferredUntil.UTC()
	}
	if !o.ValidUntil.IsZero() {
		o.ValidUntil = o.ValidUntil.UTC()
	}
	return o
}
func sameAddressOptions(a, b AddressOptions) bool {
	return a.NoDAD == b.NoDAD && a.Peer == b.Peer && a.PreferredUntil.Equal(b.PreferredUntil) && a.ValidUntil.Equal(b.ValidUntil)
}

func lifetimeRemaining(deadline, now time.Time) uint32 {
	if deadline.IsZero() {
		return math.MaxUint32
	}
	seconds := math.Ceil(deadline.Sub(now).Seconds())
	if seconds <= 0 {
		return 0
	}
	if seconds >= math.MaxUint32 {
		return math.MaxUint32 - 1
	}
	return uint32(seconds)
}

func projectAddressExpiry(d Desired, now time.Time) Desired {
	for i := range d.Interfaces {
		l := &d.Interfaces[i]
		kept := l.Addresses[:0]
		for _, address := range l.Addresses {
			o := l.AddressOptions[address]
			if !o.ValidUntil.IsZero() && !o.ValidUntil.After(now) {
				delete(l.AddressOptions, address)
				continue
			}
			kept = append(kept, address)
		}
		l.Addresses = kept
		if len(l.AddressOptions) == 0 {
			l.AddressOptions = nil
		}
	}
	return d
}

func (e *Engine) addressRecord(key addressKey) *ownedAddress {
	for i := range e.state.Addresses {
		if e.state.Addresses[i].key() == key {
			return &e.state.Addresses[i]
		}
	}
	return nil
}

func lifetimeClose(actual, expected uint32) bool {
	if actual == math.MaxUint32 || expected == math.MaxUint32 {
		return actual == expected
	}
	if actual > expected {
		return actual-expected <= 2
	}
	return expected-actual <= 2
}
func addressOptionsMatch(live AddressState, want AddressOptions, now time.Time) bool {
	return live.Peer == want.Peer && (live.Flags&addressNoDADFlag != 0) == want.NoDAD && lifetimeClose(live.PreferredLifetime, lifetimeRemaining(want.PreferredUntil, now)) && lifetimeClose(live.ValidLifetime, lifetimeRemaining(want.ValidUntil, now))
}

func (p *apply) addressConfig(l Interface, address string) error {
	key := addressKey{l.Name, address}
	options := l.AddressOptions[address]
	live, err := p.e.kernel.Interface(l.Name)
	if err != nil {
		return p.failure("address read", err)
	}
	old := p.e.addressRecord(key)
	if old != nil && old.Index > 0 && old.Index != live.Index && !p.e.ownsLink(live) {
		return p.failure("address ownership", fmt.Errorf("interface %s was replaced", l.Name))
	}
	var observed *AddressState
	for i := range live.Addresses {
		if live.Addresses[i].Address == address {
			observed = &live.Addresses[i]
			break
		}
	}
	if observed != nil {
		if old == nil {
			return p.failure("address ownership", fmt.Errorf("%s on %s already exists without ownership journal; refusing to adopt", address, l.Name))
		}
		if old.Options.NoDAD != options.NoDAD {
			return p.failure("address DAD policy", fmt.Errorf("remove and apply address %s before changing its NoDAD policy", address))
		}
		if old.Options.Peer != options.Peer {
			return p.failure("address peer", fmt.Errorf("remove and apply address %s before changing its peer", address))
		}
		now := time.Now()
		if !addressOptionsMatch(*observed, old.Options, now) && (old.Attempted == nil || !addressOptionsMatch(*observed, *old.Attempted, now)) {
			return p.failure("address option ownership", fmt.Errorf("lifetimes, DAD flags or peer for %s on %s changed outside routerd2", address, l.Name))
		}
		if addressOptionsMatch(*observed, options, now) {
			if old.Attempted != nil || old.Index != live.Index || !sameAddressOptions(old.Options, options) {
				old.Options = options
				old.Attempted = nil
				old.Index = live.Index
				return p.checkpoint("address options ownership")
			}
			return nil
		}
		if !addressOptionsMatch(*observed, old.Options, now) && old.Attempted != nil {
			// Preserve the previously acknowledged interrupted lifetime before
			// replacing its intent with this candidate or rollback.
			old.Options = *old.Attempted
		}
		old.Attempted = &options
		if err := p.checkpoint("address lifetime intent"); err != nil {
			return err
		}
		return p.change("address lifetime update", func() error { return p.e.kernel.UpdateAddress(l.Name, address, options) }, func() { old.Options = options; old.Attempted = nil; old.Index = live.Index })
	}
	return p.change("address add", func() error { return p.e.kernel.AddAddress(l.Name, address, options) }, func() {
		if old != nil {
			old.Options = options
			old.Attempted = nil
			old.Index = live.Index
			return
		}
		p.e.state.Addresses = append(p.e.state.Addresses, ownedAddress{Interface: l.Name, Address: address, Index: live.Index, Options: options})
	})
}
