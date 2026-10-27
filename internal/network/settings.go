package network

import (
	"errors"
	"fmt"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func raSetting(key settingKey) bool { return key.Kind == "accept_ra" || key.Kind == "autoconf" }

func validateSettingRecord(s ownedSetting) error {
	minimum, maximum := 0, 1
	switch s.Key.Kind {
	case "mtu":
		minimum, maximum = 68, 65535
	case "up", "autoconf":
	case "accept_ra":
		maximum = 2
	case "rp_filter_all", "rp_filter_default":
		maximum = 2
		if s.Key.Interface != "" || s.Index != 0 {
			return fmt.Errorf("invalid saved global RPF setting identity")
		}
	case "forwarding4", "forwarding6", "conntrack_tcp_liberal":
		if s.Key.Interface != "" || s.Index != 0 {
			return fmt.Errorf("invalid saved global setting identity")
		}
	default:
		return fmt.Errorf("unknown setting kind %q", s.Key.Kind)
	}
	if s.Key.Interface != "" {
		if !validName(s.Key.Interface) || s.Index < 0 || raSetting(s.Key) && s.Index == 0 {
			return fmt.Errorf("invalid saved interface setting identity")
		}
	} else if s.Key.Kind != "forwarding4" && s.Key.Kind != "forwarding6" && s.Key.Kind != "rp_filter_all" && s.Key.Kind != "rp_filter_default" && s.Key.Kind != "conntrack_tcp_liberal" {
		return fmt.Errorf("missing saved setting interface")
	}
	values := []int{s.Before, s.Last}
	if s.Attempted != nil {
		values = append(values, *s.Attempted)
	}
	for _, value := range values {
		if value < minimum || value > maximum {
			return fmt.Errorf("invalid saved %s setting", s.Key.Kind)
		}
	}
	return nil
}

func (e *Engine) settingRecord(key settingKey) *ownedSetting {
	for i := range e.state.Settings {
		if e.state.Settings[i].Key == key {
			return &e.state.Settings[i]
		}
	}
	return nil
}

func (p *apply) settingIndex(key settingKey) (int, error) {
	if key.Interface == "" {
		return 0, nil
	}
	l, err := p.e.kernel.Interface(key.Interface)
	if err != nil {
		return 0, err
	}
	return l.Index, nil
}

func settingValueOwned(s ownedSetting, value int) bool {
	return value == s.Before || value == s.Last || s.Attempted != nil && value == *s.Attempted
}

// Apply underlay MTUs before owned IPv4-in-IPv6 tunnels. Linux derives their
// MTU on creation but does not track later underlay changes. The explicit tunnel
// MTU is a ceiling; the outer IPv6 header needs another 40 bytes on its parent.
func (p *apply) interfaceMTUs(d Desired) error {
	limits := make(map[string]int)
	for _, link := range d.Links {
		if link.Kind == "ip6tnl" {
			limits[link.Name] = 0
		}
	}
	for _, iface := range d.Interfaces {
		if _, owned := limits[iface.Name]; owned {
			limits[iface.Name] = iface.MTU
		} else if iface.MTU != 0 {
			if err := p.set(settingKey{Kind: "mtu", Interface: iface.Name}, iface.MTU); err != nil {
				return err
			}
		}
	}
	for _, link := range orderedLinks(d.Links) {
		if link.Kind != "ip6tnl" {
			continue
		}
		parent, err := p.e.kernel.Interface(link.Parent)
		if err != nil {
			return p.failure("tunnel underlay MTU", err)
		}
		live, err := p.e.kernel.Interface(link.Name)
		if err != nil {
			return p.failure("tunnel MTU", err)
		}
		if err := verifyOwnedLink(live, *p.e.linkRecord(link.Name)); err != nil {
			return p.failure("tunnel MTU ownership", err)
		}
		mtu := min(parent.MTU, 65535) - 40
		if limit := limits[link.Name]; limit != 0 {
			mtu = min(mtu, limit)
		}
		// The entire created link is owned and removed on withdrawal. Its
		// derived MTU has no preexisting value to restore or journal separately.
		if live.MTU != mtu {
			if err := p.change("tunnel MTU", func() error {
				return p.e.kernel.WriteSetting(settingKey{Kind: "mtu", Interface: link.Name}, mtu)
			}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *apply) set(key settingKey, value int) error {
	if key.Kind == "up" && value == 1 {
		if link := p.e.linkRecord(key.Interface); link != nil && link.Definition.Authority != (lifetime.Deadline{}) {
			clock, err := boottime.Read()
			if err != nil {
				return p.failure("link authority", err)
			}
			if !link.Definition.Authority.Live(clock) {
				return p.failure("link authority", fmt.Errorf("link %s original authority expired before activation", key.Interface))
			}
		}
	}
	index, err := p.settingIndex(key)
	if err != nil {
		return p.failure("setting interface", err)
	}
	old := p.e.settingRecord(key)
	if old != nil && old.Index != 0 && old.Index != index {
		return p.failure("setting ownership", fmt.Errorf("interface %s was replaced; refusing to change %s", key.Interface, key.Kind))
	}
	live, err := p.e.kernel.ReadSetting(key)
	if err != nil {
		return p.failure("read "+key.Kind, err)
	}
	if old != nil && raSetting(key) && !settingValueOwned(*old, live) {
		return p.failure("setting ownership", fmt.Errorf("%s on %s changed outside routerd2", key.Kind, key.Interface))
	}
	if live == value {
		if old != nil && (old.Last != value || old.Attempted != nil || old.Index != index) {
			old.Last, old.Index, old.Attempted = value, index, nil
			return p.checkpoint("setting ownership")
		}
		// An unchanged preexisting value is not a modification to restore.
		return nil
	}
	if old == nil {
		p.e.state.Settings = append(p.e.state.Settings, ownedSetting{Key: key, Before: live, Last: live, Index: index})
		old = &p.e.state.Settings[len(p.e.state.Settings)-1]
	} else if old.Attempted != nil && live == *old.Attempted {
		old.Last = live
	}
	old.Attempted = &value
	if err := p.checkpoint("setting intent"); err != nil {
		return err
	}
	return p.change("set "+key.Kind, func() error { return p.e.kernel.WriteSetting(key, value) }, func() {
		old.Last, old.Index, old.Attempted = value, index, nil
	})
}

func (p *apply) restoreSettings(want map[settingKey]int) error {
	for i := 0; i < len(p.e.state.Settings); {
		s := &p.e.state.Settings[i]
		if _, ok := want[s.Key]; ok {
			i++
			continue
		}
		forget := func() { p.e.state.Settings = append(p.e.state.Settings[:i], p.e.state.Settings[i+1:]...) }
		index, err := p.settingIndex(s.Key)
		if errors.Is(err, errLinkNotFound) {
			forget()
			if err := p.checkpoint("forget missing link setting"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("restore setting interface", err)
		}
		if s.Index != 0 && s.Index != index {
			return p.failure("restore setting ownership", fmt.Errorf("interface %s was replaced; refusing to restore %s", s.Key.Interface, s.Key.Kind))
		}
		live, err := p.e.kernel.ReadSetting(s.Key)
		if err != nil {
			return p.failure("restore "+s.Key.Kind, err)
		}
		if !settingValueOwned(*s, live) {
			return p.failure("restore "+s.Key.Kind, fmt.Errorf("%s changed outside routerd2: have %d, last set %d; refusing to overwrite", s.Key.Interface, live, s.Last))
		}
		if live != s.Before {
			if s.Attempted != nil && live == *s.Attempted {
				s.Last = live
			}
			before := s.Before
			s.Attempted = &before
			if err := p.checkpoint("setting restore intent"); err != nil {
				return err
			}
			if err := p.change("restore "+s.Key.Kind, func() error { return p.e.kernel.WriteSetting(s.Key, s.Before) }, forget); err != nil {
				return err
			}
		} else {
			forget()
			if err := p.checkpoint("restore setting journal"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *apply) interfaceRASettings(l Interface) error {
	if l.IPv6AcceptRA != nil {
		if err := p.set(settingKey{Kind: "accept_ra", Interface: l.Name}, *l.IPv6AcceptRA); err != nil {
			return err
		}
	}
	if l.IPv6Autoconf != nil {
		if err := p.set(settingKey{Kind: "autoconf", Interface: l.Name}, boolInt(*l.IPv6Autoconf)); err != nil {
			return err
		}
	}
	return nil
}
