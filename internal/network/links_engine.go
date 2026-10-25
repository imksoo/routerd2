package network

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strings"
)

type ownedLink struct {
	announceAt uint64 // transient send schedule; restart reannounces current ownership
	Definition Link   `json:"definition"`
	Index      int    `json:"index"`
	Token      string `json:"token"`
	// Attempted is persisted before a multi-attribute link update. It allows a
	// restart or rollback to finish an interrupted update without authorizing an
	// unrelated live value. Creation still requires exclusive netlink creation.
	Attempted *Link `json:"attempted,omitempty"`
}

type ownedAttribute struct {
	Key            settingKey `json:"key"`
	Index          int        `json:"index"`
	Before         string     `json:"before"`
	Last           string     `json:"last"`
	BeforeIndex    int        `json:"before_index,omitempty"`
	LastIndex      int        `json:"last_index,omitempty"`
	Attempted      *string    `json:"attempted,omitempty"`
	AttemptedIndex int        `json:"attempted_index,omitempty"`
}

func validateLinkJournal(s journal) error {
	seen := map[string]bool{}
	for _, l := range s.Links {
		if seen[l.Definition.Name] {
			return fmt.Errorf("duplicate saved link %s", l.Definition.Name)
		}
		seen[l.Definition.Name] = true
		if err := Validate(Desired{Links: []Link{l.Definition}}); err != nil {
			return err
		}
		token, err := hex.DecodeString(l.Token)
		if err != nil || len(token) != 16 || l.Index < 1 {
			return fmt.Errorf("invalid saved link identity for %s", l.Definition.Name)
		}
		if l.Attempted != nil {
			if err := Validate(Desired{Links: []Link{*l.Attempted}}); err != nil {
				return err
			}
			if !sameLinkIdentity(l.Definition, *l.Attempted) {
				return fmt.Errorf("saved link %s changed immutable identity", l.Definition.Name)
			}
		}
	}
	seenAttributes := map[settingKey]bool{}
	for _, a := range s.Attributes {
		if a.Index < 1 || !validName(a.Key.Interface) || seenAttributes[a.Key] {
			return fmt.Errorf("invalid saved link attribute")
		}
		seenAttributes[a.Key] = true
		if a.Attempted == nil && a.AttemptedIndex != 0 {
			return fmt.Errorf("invalid saved attribute intent")
		}
		switch a.Key.Kind {
		case "alias":
			if !validAlias(a.Before, 255) || !validAlias(a.Last, 255) || a.BeforeIndex != 0 || a.LastIndex != 0 {
				return fmt.Errorf("invalid saved alias")
			}
			if a.Attempted != nil && (!validAlias(*a.Attempted, 255) || a.AttemptedIndex != 0) {
				return fmt.Errorf("invalid saved alias intent")
			}
		case "master":
			for _, ref := range []struct {
				name  string
				index int
			}{{a.Before, a.BeforeIndex}, {a.Last, a.LastIndex}} {
				if ref.name != "" && (!validName(ref.name) || ref.index < 1) || ref.name == "" && ref.index != 0 {
					return fmt.Errorf("invalid saved master")
				}
			}
			if a.Attempted != nil && (*a.Attempted != "" && (!validName(*a.Attempted) || a.AttemptedIndex < 1) || *a.Attempted == "" && a.AttemptedIndex != 0) {
				return fmt.Errorf("invalid saved master intent")
			}
		default:
			return fmt.Errorf("unknown saved attribute %q", a.Key.Kind)
		}
	}
	return nil
}

func (e *Engine) linkRecord(name string) *ownedLink {
	for i := range e.state.Links {
		if e.state.Links[i].Definition.Name == name {
			return &e.state.Links[i]
		}
	}
	return nil
}

func (e *Engine) ownsLink(l InterfaceState) bool {
	o := e.linkRecord(l.Name)
	return o != nil && l.Index == o.Index && l.Kind == o.Definition.Kind && strings.HasPrefix(l.Alias, linkAliasPrefix+o.Token+":")
}

func verifyOwnedLink(live InterfaceState, saved ownedLink) error {
	if live.Index != saved.Index || live.Link == nil || !strings.HasPrefix(live.Alias, linkAliasPrefix+saved.Token+":") {
		return fmt.Errorf("link %s exists without the recorded index, kind and ownership marker", saved.Definition.Name)
	}
	if !sameLinkIdentity(*live.Link, saved.Definition) {
		return fmt.Errorf("link %s identity changed outside routerd2", saved.Definition.Name)
	}
	// Each field must equal the last acknowledged value or a durably recorded
	// interrupted update. Accepting arbitrary live values would erase drift.
	a, b, observed := saved.Definition, saved.Definition, *live.Link
	if saved.Attempted != nil {
		b = *saved.Attempted
	}
	av, bv, lv := reflect.ValueOf(a), reflect.ValueOf(b), reflect.ValueOf(observed)
	for i := 0; i < av.NumField(); i++ {
		field := av.Type().Field(i).Name
		if field == "BootID" || field == "ExpiresNanos" {
			continue
		}
		if field == "HardwareAddr" && a.HardwareAddr == "" {
			continue
		}
		if field == "IPv6AddrGenMode" && a.IPv6AddrGenMode == "" {
			continue
		}
		if !reflect.DeepEqual(lv.Field(i).Interface(), av.Field(i).Interface()) && !reflect.DeepEqual(lv.Field(i).Interface(), bv.Field(i).Interface()) {
			return fmt.Errorf("link %s field %s changed outside routerd2", a.Name, field)
		}
	}
	return nil
}

func (p *apply) preflightLinks(d Desired) error {
	want := map[string]Link{}
	for _, l := range d.Links {
		want[l.Name] = l
	}
	for _, old := range p.e.state.Links {
		if next, keep := want[old.Definition.Name]; keep && !sameLinkIdentity(old.Definition, next) {
			return p.failure("link identity", fmt.Errorf("%s: kind, parent, IDs, UDP port or macvlan mode changed; remove and apply before recreating", next.Name))
		}
		live, err := p.e.kernel.Interface(old.Definition.Name)
		if errors.Is(err, errLinkNotFound) {
			continue
		}
		if err != nil {
			return p.failure("link ownership read", err)
		}
		if err := verifyOwnedLink(live, old); err != nil {
			return p.failure("link ownership", err)
		}
		if _, keep := want[old.Definition.Name]; !keep {
			for _, i := range d.Interfaces {
				if i.Name == old.Definition.Name || i.Master != nil && *i.Master == old.Definition.Name {
					return p.failure("link removal", fmt.Errorf("remove references to %s before deleting its link", old.Definition.Name))
				}
			}
			for _, l := range d.Links {
				if l.Parent == old.Definition.Name {
					return p.failure("link removal", fmt.Errorf("%s is still the parent of %s", old.Definition.Name, l.Name))
				}
			}
			for _, t := range d.DSLite {
				if t.Underlay == old.Definition.Name {
					return p.failure("link removal", fmt.Errorf("%s is still the underlay of %s", old.Definition.Name, t.Name))
				}
			}
			for _, r := range d.Routes {
				if routeUsesInterface(r, old.Definition.Name) {
					return p.failure("link removal", fmt.Errorf("%s is still referenced by a route", old.Definition.Name))
				}
			}
		}
	}
	for _, l := range d.Links {
		if p.e.linkRecord(l.Name) != nil {
			continue
		}
		if _, err := p.e.kernel.Interface(l.Name); err == nil {
			return p.failure("link ownership", fmt.Errorf("link %s already exists without an ownership journal; refusing to adopt", l.Name))
		} else if !errors.Is(err, errLinkNotFound) {
			return p.failure("link read", err)
		}
	}
	return nil
}

func (p *apply) links(want []Link) error {
	for _, l := range orderedLinks(want) {
		live, err := p.e.kernel.Interface(l.Name)
		if errors.Is(err, errLinkNotFound) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				return p.failure("link token", err)
			}
			token := hex.EncodeToString(bytes[:])
			var created InterfaceState
			if err := p.change("link create", func() error {
				if err := p.e.kernel.CreateLink(l, virtualLinkAlias(token, l)); err != nil {
					return err
				}
				created, err = p.e.kernel.Interface(l.Name)
				if err != nil {
					return errors.Join(err, p.e.kernel.DeleteLink(l.Name))
				}
				if created.Index < 1 || created.Link == nil || !sameManagedLink(*created.Link, l) || created.Alias != virtualLinkAlias(token, l) {
					return errors.Join(fmt.Errorf("link %s readback differs from requested definition: %+v", l.Name, created.Link), p.e.kernel.DeleteLink(l.Name))
				}
				return nil
			}, func() {
				record := ownedLink{Definition: l, Index: created.Index, Token: token}
				if old := p.e.linkRecord(l.Name); old != nil {
					*old = record
					// The old object vanished. Its per-field before-images do not
					// describe this newly and exclusively created link.
					settings := p.e.state.Settings[:0]
					for _, s := range p.e.state.Settings {
						if s.Key.Interface != l.Name {
							settings = append(settings, s)
						}
					}
					p.e.state.Settings = settings
					attributes := p.e.state.Attributes[:0]
					for _, a := range p.e.state.Attributes {
						if a.Key.Interface != l.Name {
							attributes = append(attributes, a)
						}
					}
					p.e.state.Attributes = attributes
				} else {
					p.e.state.Links = append(p.e.state.Links, record)
				}
			}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("link read", err)
		}
		old := p.e.linkRecord(l.Name)
		if old == nil {
			return p.failure("link ownership", fmt.Errorf("link %s is unowned", l.Name))
		}
		if err := verifyOwnedLink(live, *old); err != nil {
			return p.failure("link ownership", err)
		}
		if sameManagedLink(*live.Link, l) {
			if !reflect.DeepEqual(old.Definition, l) || old.Attempted != nil {
				old.Definition = l
				old.Attempted = nil
				// Apply persists all metadata-only renewals before acknowledging them.
			}
			continue
		}
		// An earlier interrupted update may have left a mixture of its two
		// journaled definitions. Record that verified live state before changing
		// the intent, so another interruption during rollback remains recoverable.
		current := *live.Link
		current.BootID, current.ExpiresNanos = old.Definition.BootID, old.Definition.ExpiresNanos
		if old.Definition.HardwareAddr == "" {
			current.HardwareAddr = ""
		}
		if old.Definition.IPv6AddrGenMode == "" {
			current.IPv6AddrGenMode = ""
		}
		old.Definition = current
		old.Attempted = &l
		if err := p.checkpoint("link update intent"); err != nil {
			return err
		}
		if err := p.change("link update", func() error {
			if err := p.e.kernel.UpdateLink(l, virtualLinkAlias(old.Token, l)); err != nil {
				return err
			}
			updated, err := p.e.kernel.Interface(l.Name)
			if err != nil {
				return err
			}
			if updated.Index != old.Index || updated.Link == nil || !sameManagedLink(*updated.Link, l) {
				return fmt.Errorf("link %s update was not acknowledged by kernel readback", l.Name)
			}
			return nil
		}, func() { old.Definition = l; old.Attempted = nil }); err != nil {
			return err
		}
	}
	return nil
}

func attributeValue(l InterfaceState, kind string) (string, int, error) {
	switch kind {
	case "alias":
		return l.Alias, 0, nil
	case "master":
		return l.Master, l.MasterIndex, nil
	default:
		return "", 0, fmt.Errorf("unknown link attribute %q", kind)
	}
}

func attributeMatchesSaved(current string, index int, a ownedAttribute) bool {
	if current == a.Last && index == a.LastIndex || current == a.Before && index == a.BeforeIndex {
		return true
	}
	if a.Attempted == nil {
		return false
	}
	if current == *a.Attempted && index == a.AttemptedIndex {
		return true
	}
	// Linux changing a master can detach the old master before the new master's
	// add_slave callback fails. Only a persisted in-flight master operation
	// authorizes recovery from that intermediate detached state.
	return a.Key.Kind == "master" && current == "" && index == 0
}

func (p *apply) attribute(key settingKey, value string) error {
	live, err := p.e.kernel.Interface(key.Interface)
	if err != nil {
		return p.failure("attribute read", err)
	}
	current, currentIndex, err := attributeValue(live, key.Kind)
	if err != nil {
		return p.failure("attribute read", err)
	}
	wantedIndex := 0
	if key.Kind == "master" && value != "" {
		master, err := p.e.kernel.Interface(value)
		if err != nil {
			return p.failure("master read", err)
		}
		if master.Kind != "bridge" && master.Kind != "vrf" {
			return p.failure("master kind", fmt.Errorf("%s is not a bridge or VRF", value))
		}
		wantedIndex = master.Index
	}
	var old *ownedAttribute
	for i := range p.e.state.Attributes {
		if p.e.state.Attributes[i].Key == key {
			old = &p.e.state.Attributes[i]
			break
		}
	}
	if old != nil {
		if live.Index != old.Index || !attributeMatchesSaved(current, currentIndex, *old) {
			return p.failure("attribute ownership", fmt.Errorf("%s %s changed outside routerd2", key.Interface, key.Kind))
		}
	}
	if current == value && currentIndex == wantedIndex {
		if old != nil && old.Attempted != nil {
			old.Last = value
			old.LastIndex = wantedIndex
			old.Attempted = nil
			old.AttemptedIndex = 0
			return p.checkpoint("attribute acknowledgment")
		}
		return nil
	}
	if old == nil {
		if key.Kind == "alias" && strings.HasPrefix(current, "routerd2:") {
			return p.failure("alias ownership", fmt.Errorf("%s has a reserved ownership alias", key.Interface))
		}
		p.e.state.Attributes = append(p.e.state.Attributes, ownedAttribute{Key: key, Index: live.Index, Before: current, Last: current, BeforeIndex: currentIndex, LastIndex: currentIndex})
		old = &p.e.state.Attributes[len(p.e.state.Attributes)-1]
	}
	old.Last = current
	old.LastIndex = currentIndex
	old.Attempted = &value
	old.AttemptedIndex = wantedIndex
	if err := p.checkpoint("attribute update intent"); err != nil {
		return err
	}
	return p.change("set "+key.Kind, func() error { return p.e.kernel.WriteAttribute(key, value) }, func() {
		old.Last = value
		old.LastIndex = wantedIndex
		old.Attempted = nil
		old.AttemptedIndex = 0
	})
}

func (p *apply) restoreAttributes(d Desired) error {
	want := map[settingKey]bool{}
	for _, l := range d.Interfaces {
		if l.Master != nil {
			want[settingKey{Kind: "master", Interface: l.Name}] = true
		}
		if l.Alias != nil {
			want[settingKey{Kind: "alias", Interface: l.Name}] = true
		}
	}
	for i := 0; i < len(p.e.state.Attributes); {
		a := p.e.state.Attributes[i]
		if want[a.Key] {
			i++
			continue
		}
		live, err := p.e.kernel.Interface(a.Key.Interface)
		forget := func() { p.e.state.Attributes = append(p.e.state.Attributes[:i], p.e.state.Attributes[i+1:]...) }
		if errors.Is(err, errLinkNotFound) {
			forget()
			if err := p.checkpoint("forget absent link attribute"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("restore attribute read", err)
		}
		current, currentIndex, err := attributeValue(live, a.Key.Kind)
		if err != nil {
			return p.failure("restore attribute read", err)
		}
		if live.Index != a.Index || !attributeMatchesSaved(current, currentIndex, a) {
			return p.failure("restore attribute ownership", fmt.Errorf("%s %s changed outside routerd2; refusing to overwrite", a.Key.Interface, a.Key.Kind))
		}
		if a.Key.Kind == "master" && a.Before != "" {
			before, err := p.e.kernel.Interface(a.Before)
			if err != nil {
				return p.failure("restore original master", err)
			}
			if before.Index != a.BeforeIndex {
				return p.failure("restore original master", fmt.Errorf("original master %s was replaced", a.Before))
			}
		}
		if current != a.Before || currentIndex != a.BeforeIndex {
			p.e.state.Attributes[i].Last = current
			p.e.state.Attributes[i].LastIndex = currentIndex
			p.e.state.Attributes[i].Attempted = &a.Before
			p.e.state.Attributes[i].AttemptedIndex = a.BeforeIndex
			if err := p.checkpoint("attribute restore intent"); err != nil {
				return err
			}
			if err := p.change("restore "+a.Key.Kind, func() error { return p.e.kernel.WriteAttribute(a.Key, a.Before) }, forget); err != nil {
				return err
			}
		} else {
			forget()
			if err := p.checkpoint("restore attribute journal"); err != nil {
				return err
			}
		}
	}
	return nil
}

func automaticLinkLocal(l InterfaceState) string {
	if l.Link != nil && l.Link.IPv6AddrGenMode != "" && l.Link.IPv6AddrGenMode != "eui64" {
		return ""
	}
	mac, err := net.ParseMAC(l.HardwareAddr)
	if err != nil || len(mac) != 6 || bytesAllZero(mac) {
		return ""
	}
	var ip [16]byte
	ip[0], ip[1] = 0xfe, 0x80
	ip[8], ip[9], ip[10], ip[11], ip[12], ip[13], ip[14], ip[15] = mac[0]^2, mac[1], mac[2], 0xff, 0xfe, mac[3], mac[4], mac[5]
	return netip.AddrFrom16(ip).String() + "/64"
}

func (p *apply) removeLinks(d Desired) error {
	wanted := map[string]bool{}
	for _, l := range d.Links {
		wanted[l.Name] = true
	}
	// Creation order is parent-before-child. Use a topological order again after
	// restart instead of relying on the order of journal entries.
	var definitions []Link
	for _, o := range p.e.state.Links {
		definitions = append(definitions, o.Definition)
	}
	ordered := orderedLinks(definitions)
	for n := len(ordered) - 1; n >= 0; n-- {
		name := ordered[n].Name
		if wanted[name] {
			continue
		}
		old := p.e.linkRecord(name)
		if old == nil {
			continue
		}
		forget := func() {
			p.forgetWireGuard(name)
			for i := range p.e.state.Links {
				if p.e.state.Links[i].Definition.Name == name {
					p.e.state.Links = append(p.e.state.Links[:i], p.e.state.Links[i+1:]...)
					return
				}
			}
		}
		live, err := p.e.kernel.Interface(name)
		if errors.Is(err, errLinkNotFound) {
			forget()
			if err := p.checkpoint("forget absent link"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("link removal read", err)
		}
		if err := verifyOwnedLink(live, *old); err != nil {
			return p.failure("link removal ownership", err)
		}
		if live.Master != "" {
			return p.failure("link removal", fmt.Errorf("link %s is still a member of %s", name, live.Master))
		}
		all, err := p.e.kernel.Interfaces()
		if err != nil {
			return p.failure("link references", err)
		}
		for _, other := range all {
			if other.Name != name && (other.Parent == name || other.Master == name) {
				return p.failure("link removal", fmt.Errorf("link %s is still referenced by %s", name, other.Name))
			}
		}
		linkLocal := automaticLinkLocal(live)
		for _, a := range live.Addresses {
			if a.Address != linkLocal {
				return p.failure("link removal", fmt.Errorf("link %s still has unowned address %s", name, a.Address))
			}
		}
		rs, err := p.e.kernel.Routes(0)
		if err != nil {
			return p.failure("link route references", err)
		}
		for _, r := range rs {
			if !routeUsesInterface(r.Route, name) {
				continue
			}
			// Linux creates these routes as a consequence of enabling IPv6 on a
			// newly owned Ethernet link. They are intrinsic to that link; all other
			// addresses/routes, including static link-local routes, block deletion.
			intrinsic := linkLocal != "" && r.Gateway == "" && !r.Advanced && !r.Multipath && (r.Protocol == 2 || r.Protocol == 3) && (r.Destination == "fe80::/64" || r.Destination == "ff00::/8" || r.Destination == addressIP(linkLocal)+"/128")
			if !intrinsic {
				return p.failure("link removal", fmt.Errorf("link %s still has route %s table %d", name, r.Destination, r.Table))
			}
		}
		if err := p.change("link remove", func() error { return p.e.kernel.DeleteLink(name) }, forget); err != nil {
			return err
		}
	}
	return nil
}
