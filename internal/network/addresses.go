package network

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// AddressOptions are explicit per-address options. A zero deadline means
// infinite lifetime. A past PreferredUntil deprecates the address while keeping
// it valid; a passed ValidUntil removes it from the projected Desired. Linux
// also expires finite addresses autonomously if the controller is unavailable.
// NoDAD is only for explicitly coordinated IPv6 addresses such as HA VIPs. It
// never changes the DAD policy of another address on the same interface.
type AddressOptions struct {
	BootLifetime   BootLifetime `json:"boot_lifetime,omitzero"`
	PreferredUntil time.Time    `json:"preferred_until,omitzero"`
	ValidUntil     time.Time    `json:"valid_until,omitzero"`
	NoDAD          bool         `json:"no_dad,omitempty"`
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
			if b := o.BootLifetime; b != (BootLifetime{}) {
				if !b.valid() || !o.PreferredUntil.IsZero() || !o.ValidUntil.IsZero() {
					errs = append(errs, fmt.Errorf("interface %s address %s: invalid boot lifetime", l.Name, p))
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
	return a.BootLifetime == b.BootLifetime && a.NoDAD == b.NoDAD && a.Peer == b.Peer && a.PreferredUntil.Equal(b.PreferredUntil) && a.ValidUntil.Equal(b.ValidUntil)
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
	p, v, err := addressLifetimes(want, now)
	return err == nil && live.Peer == want.Peer && (live.Flags&addressNoDADFlag != 0) == want.NoDAD && lifetimeClose(live.PreferredLifetime, p) && lifetimeClose(live.ValidLifetime, v)
}

// Read each interface once and persist all pending lifetime changes before
// updating its addresses. Apply's completion (or failure) saves the acknowledged
// results; a restart can still match either timer for an interrupted update.
func (p *apply) addressConfigs(interfaces []Interface) error {
	type update struct {
		interfaceName string
		index         int
		address       string
		options       AddressOptions
		owned         *ownedAddress
	}
	var updates []update
	var additions []update
	for _, l := range interfaces {
		if len(l.Addresses) == 0 {
			continue
		}
		live, err := p.e.kernel.Interface(l.Name)
		if err != nil {
			return p.failure("address read", err)
		}
		for _, address := range l.Addresses {
			options := l.AddressOptions[address]
			old := p.e.addressRecord(addressKey{l.Name, address})
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
			if observed == nil {
				additions = append(additions, update{interfaceName: l.Name, index: live.Index, address: address, options: options})
				continue
			}
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
			matches := addressOptionsMatch(*observed, old.Options, now)
			if !matches && (old.Attempted == nil || !addressOptionsMatch(*observed, *old.Attempted, now)) {
				return p.failure("address option ownership", fmt.Errorf("lifetimes, DAD flags or peer for %s on %s changed outside routerd2", address, l.Name))
			}
			// Rounded kernel lifetimes do not make different absolute deadlines equal.
			if sameAddressOptions(old.Options, options) && matches {
				old.Attempted = nil
				old.Index = live.Index
				continue
			}
			if !matches && old.Attempted != nil {
				old.Options = *old.Attempted
			}
			updates = append(updates, update{interfaceName: l.Name, index: live.Index, address: address, options: options, owned: old})
		}
	}
	if len(updates) > 0 {
		for i := range updates {
			updates[i].owned.Attempted = &updates[i].options
		}
		if err := p.checkpoint("address lifetime intents"); err != nil {
			return err
		}
		for _, update := range updates {
			if err := p.mutate("address lifetime update", func() error {
				return p.e.kernel.UpdateAddress(update.interfaceName, update.address, update.options)
			}); err != nil {
				return err
			}
			update.owned.Options = update.options
			update.owned.Attempted = nil
			update.owned.Index = update.index
		}
	}
	// Additions can grow the ownership slice. Complete updates using its old
	// records first, then look up each new address after any previous append.
	for _, addition := range additions {
		if err := p.change("address add", func() error {
			return p.e.kernel.AddAddress(addition.interfaceName, addition.address, addition.options)
		}, func() {
			if old := p.e.addressRecord(addressKey{addition.interfaceName, addition.address}); old != nil {
				old.Options = addition.options
				old.Attempted = nil
				old.Index = addition.index
			} else {
				p.e.state.Addresses = append(p.e.state.Addresses, ownedAddress{Interface: addition.interfaceName, Address: addition.address, Index: addition.index, Options: addition.options})
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// A lease on a virtual link also bounds addresses managed on that link. Keep
// the tighter protocol/UTC deadline; operator intent remains in the caller's
// unmodified Desired. The writer can then revoke local routes as well as L2.
func boundLinkAddresses(d *Desired, clock boottime.Clock) {
	var lifetimes map[string]lifetime.Deadline
	for _, link := range d.Links {
		if link.Authority == (lifetime.Deadline{}) {
			continue
		}
		if lifetimes == nil {
			lifetimes = make(map[string]lifetime.Deadline)
		}
		lifetimes[link.Name] = link.Authority
	}
	for i := range d.Interfaces {
		iface := &d.Interfaces[i]
		owner, finite := lifetimes[iface.Name]
		if !finite {
			continue
		}
		if iface.AddressOptions == nil {
			iface.AddressOptions = make(map[string]AddressOptions)
		}
		for _, address := range iface.Addresses {
			options := iface.AddressOptions[address]
			if owner.BootID != "" {
				if owner.Nanos != 0 {
					options = BoundAddressLifetime(options, clock, owner.Nanos)
				}
			} else if !owner.UTC.IsZero() && options.BootLifetime != (BootLifetime{}) {
				deadline := uint64(clock.BoottimeNanoseconds) + uint64(max(0, owner.UTC.Sub(clock.WallTime)))
				options = BoundAddressLifetime(options, clock, deadline)
			} else if !owner.UTC.IsZero() {
				if options.ValidUntil.IsZero() || options.ValidUntil.After(owner.UTC) {
					options.ValidUntil = owner.UTC
				}
				if options.PreferredUntil.IsZero() || options.PreferredUntil.After(owner.UTC) {
					options.PreferredUntil = owner.UTC
				}
			}
			iface.AddressOptions[address] = options
		}
	}
}

// BoundAddressLifetime intersects an address's validated lifetime with an
// original condition or link deadline on the same boot clock. UTC intent is
// converted at this projection's clock; the returned value owns no new lease.
func BoundAddressLifetime(options AddressOptions, clock boottime.Clock, deadline uint64) AddressOptions {
	until := BootLifetime{BootID: clock.BootID, PreferredNanos: deadline, ValidNanos: deadline}
	if own := options.BootLifetime; own != (BootLifetime{}) {
		until.ValidNanos = min(until.ValidNanos, own.ValidNanos)
		until.PreferredNanos = min(until.PreferredNanos, own.PreferredNanos)
	} else {
		bound := func(wall time.Time) uint64 {
			if wall.IsZero() {
				return deadline
			}
			return min(deadline, uint64(clock.BoottimeNanoseconds)+uint64(max(0, wall.Sub(clock.WallTime))))
		}
		until.ValidNanos = bound(options.ValidUntil)
		until.PreferredNanos = bound(options.PreferredUntil)
		options.ValidUntil, options.PreferredUntil = time.Time{}, time.Time{}
	}
	until.PreferredNanos = min(until.PreferredNanos, until.ValidNanos)
	options.BootLifetime = until
	return options
}
