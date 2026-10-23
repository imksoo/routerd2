package network

import (
	"errors"
	"fmt"
	"net/netip"
)

// VRRPGuardTable contains only VRRP Accept_Mode input restrictions. It is
// independent of the routing/forward/NAT table and installs no forward policy.
const VRRPGuardTable = "routerd2_ha"

// VRRPGuard prevents a non-owner MASTER with Accept_Mode=false from accepting
// data addressed to its VIPs. Addresses are bare IP literals declared on the
// referenced Interface in this same Desired. All ingress paths are covered;
// Interface establishes VIP ownership, not an ingress-only restriction.
// IPv6 Neighbor Solicitation/Advertisement and IP protocol 112 remain allowed.
type VRRPGuard struct {
	Family    int      `json:"family"`
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses"`
}

func validateVRRPGuards(d Desired) error {
	var errs []error
	declared := map[string]map[string]bool{}
	for _, l := range d.Interfaces {
		declared[l.Name] = map[string]bool{}
		for _, a := range l.Addresses {
			if ip := addressIP(a); ip != "" {
				declared[l.Name][ip] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, g := range d.VRRPGuards {
		if !validName(g.Interface) || (g.Family != 4 && g.Family != 6) || len(g.Addresses) == 0 {
			errs = append(errs, fmt.Errorf("VRRP guard requires family 4/6, interface and at least one VIP"))
		}
		for _, s := range g.Addresses {
			a, err := netip.ParseAddr(s)
			if err != nil || a.Is4In6() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() || a.Is4() != (g.Family == 4) {
				errs = append(errs, fmt.Errorf("VRRP guard %s: %q must be a same-family bare unicast address", g.Interface, s))
				continue
			}
			if !declared[g.Interface][a.String()] {
				errs = append(errs, fmt.Errorf("VRRP guard %s: VIP %s is not declared on this interface", g.Interface, a))
			}
			if seen[a.String()] {
				errs = append(errs, fmt.Errorf("duplicate VRRP guard VIP %s", a))
			}
			seen[a.String()] = true
		}
	}
	return errors.Join(errs...)
}

func (p *apply) preflightVRRPGuards(want []VRRPGuard) error {
	if len(want) == 0 && !p.e.state.VRRPGuardOwned {
		return nil
	}
	live, err := p.e.kernel.VRRPGuard()
	if err != nil {
		return p.failure("VRRP guard read", err)
	}
	if live.Present && (!live.Owned || !p.e.state.VRRPGuardOwned) {
		return p.failure("VRRP guard ownership", fmt.Errorf("inet table %s exists without matching ownership journal and marker", VRRPGuardTable))
	}
	return nil
}

func (p *apply) vrrpGuards(want []VRRPGuard) error {
	if len(want) == 0 && !p.e.state.VRRPGuardOwned {
		return nil
	}
	if err := p.preflightVRRPGuards(want); err != nil {
		return err
	}
	for _, g := range want {
		live, err := p.e.kernel.Interface(g.Interface)
		if err != nil {
			return p.failure("VRRP guard VIP read", err)
		}
		for _, vip := range g.Addresses {
			found := false
			for _, a := range live.Addresses {
				if addressIP(a.Address) == vip {
					saved := p.e.addressRecord(addressKey{g.Interface, a.Address})
					found = saved != nil && (saved.Index == 0 || saved.Index == live.Index)
					break
				}
			}
			if !found {
				return p.failure("VRRP guard VIP ownership", fmt.Errorf("VIP %s on %s is absent or unowned", vip, g.Interface))
			}
		}
	}
	return p.change("VRRP guard atomic replace", func() error { return p.e.kernel.ReplaceVRRPGuard(want) }, func() { p.e.state.VRRPGuardOwned = len(want) > 0 })
}
