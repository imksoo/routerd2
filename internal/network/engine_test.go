//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeKernel struct {
	links                        map[string]InterfaceState
	arpIgnore                    map[string]int
	arpAnnounce                  map[string]int
	routes                       []RouteState
	rules                        []PolicyRuleState
	forward4, forward6           int
	rpFilterAll, rpFilterDefault int
	firewall                     FirewallState
	guard                        FirewallState
	fail                         string
	writes                       []string
}

func (f *fakeKernel) AnnounceLink(linkAnnouncement) error { return f.op("link-announcement") }

func fake() *fakeKernel {
	return &fakeKernel{links: map[string]InterfaceState{"lan0": {Name: "lan0", Index: 1, Kind: "dummy", MTU: 1500}, "wan0": {Name: "wan0", Index: 2, Kind: "dummy", MTU: 1500}}, arpIgnore: make(map[string]int), arpAnnounce: make(map[string]int), firewall: FirewallState{Table: FirewallTable}}
}
func (f *fakeKernel) op(name string) error {
	if f.fail == name {
		return fmt.Errorf("injected %s failure", name)
	}
	f.writes = append(f.writes, name)
	return nil
}
func (f *fakeKernel) Interface(name string) (InterfaceState, error) {
	l, ok := f.links[name]
	if !ok {
		return l, errLinkNotFound
	}
	l.Addresses = append([]AddressState(nil), l.Addresses...)
	return l, nil
}
func (f *fakeKernel) AddAddress(name, address string, options AddressOptions) error {
	if err := f.op("address-add"); err != nil {
		return err
	}
	l := f.links[name]
	for _, a := range l.Addresses {
		if a.Address == address {
			return errors.New("duplicate address")
		}
	}
	now := time.Now()
	preferred, valid, err := addressLifetimes(options, now)
	if err != nil {
		return err
	}
	a := AddressState{Address: address, Peer: options.Peer, PreferredLifetime: preferred, ValidLifetime: valid}
	if options.NoDAD {
		a.Flags |= addressNoDADFlag
	}
	l.Addresses = append(l.Addresses, a)
	f.links[name] = l
	return nil
}

func (f *fakeKernel) UpdateAddress(name, address string, options AddressOptions) error {
	if err := f.op("address-update"); err != nil {
		return err
	}
	l, ok := f.links[name]
	if !ok {
		return errLinkNotFound
	}
	for i := range l.Addresses {
		if l.Addresses[i].Address == address {
			l.Addresses[i].Peer = options.Peer
			now := time.Now()
			preferred, valid, err := addressLifetimes(options, now)
			if err != nil {
				return err
			}
			l.Addresses[i].PreferredLifetime = preferred
			l.Addresses[i].ValidLifetime = valid
			if options.NoDAD {
				l.Addresses[i].Flags |= addressNoDADFlag
			} else {
				l.Addresses[i].Flags &^= addressNoDADFlag
			}
			f.links[name] = l
			return nil
		}
	}
	return errors.New("address missing")
}
func (f *fakeKernel) DeleteAddress(name, address string, options AddressOptions) error {
	if err := f.op("address-delete"); err != nil {
		return err
	}
	l := f.links[name]
	for i, a := range l.Addresses {
		if a.Address == address {
			if a.Peer != options.Peer {
				return errors.New("address peer mismatch")
			}
			l.Addresses = append(l.Addresses[:i], l.Addresses[i+1:]...)
			break
		}
	}
	f.links[name] = l
	return nil
}
func (f *fakeKernel) Routes(table int) ([]RouteState, error) {
	var out []RouteState
	for _, r := range f.routes {
		if table == 0 || r.Table == table {
			r.Route = normalizeRoute(r.Route)
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeKernel) AddRoute(saved ownedRoute) error {
	r := saved.Route
	if err := f.op("route-add"); err != nil {
		return err
	}
	r = normalizeRoute(r)
	f.routes = append(f.routes, RouteState{Route: r, kernelExpiry: saved.KernelExpiry, Protocol: RouteProtocol, Type: 1, Multipath: len(r.NextHops) > 0})
	return nil
}
func (f *fakeKernel) UpdateRoute(saved ownedRoute) error {
	if err := f.op("route-update"); err != nil {
		return err
	}
	updated := normalizeRoute(saved.Route)
	for i, r := range f.routes {
		if routeSlot(r.Route) == routeSlot(saved.Route) {
			f.routes[i] = RouteState{Route: updated, kernelExpiry: saved.KernelExpiry, Protocol: RouteProtocol, Type: 1, Multipath: len(updated.NextHops) > 0}
			return nil
		}
	}
	return errors.New("missing route")
}
func (f *fakeKernel) DeleteRoute(r Route) error {
	if err := f.op("route-delete"); err != nil {
		return err
	}
	for i, v := range f.routes {
		if sameKernelRoute(v.Route, r) {
			f.routes = append(f.routes[:i], f.routes[i+1:]...)
			return nil
		}
	}
	return errors.New("missing route")
}
func (f *fakeKernel) ReadSetting(k settingKey) (int, error) {
	switch k.Kind {
	case "forwarding4":
		return f.forward4, nil
	case "forwarding6":
		return f.forward6, nil
	case "rp_filter_all":
		return f.rpFilterAll, nil
	case "rp_filter_default":
		return f.rpFilterDefault, nil
	}
	l, err := f.Interface(k.Interface)
	if err != nil {
		return 0, err
	}
	if k.Kind == "mtu" {
		return l.MTU, nil
	}
	if k.Kind == "arp_ignore" {
		return f.arpIgnore[k.Interface], nil
	}
	if k.Kind == "arp_announce" {
		return f.arpAnnounce[k.Interface], nil
	}
	if k.Kind == "accept_ra" {
		if l.IPv6AcceptRA == nil {
			return 1, nil
		}
		return *l.IPv6AcceptRA, nil
	}
	if k.Kind == "autoconf" {
		if l.IPv6Autoconf == nil {
			return 1, nil
		}
		return boolInt(*l.IPv6Autoconf), nil
	}
	return boolInt(l.Up), nil
}
func (f *fakeKernel) WriteSetting(k settingKey, v int) error {
	if err := f.op("setting-" + k.Kind); err != nil {
		return err
	}
	switch k.Kind {
	case "forwarding4":
		f.forward4 = v
	case "forwarding6":
		f.forward6 = v
	case "rp_filter_all":
		f.rpFilterAll = v
	case "rp_filter_default":
		f.rpFilterDefault = v
	default:
		l := f.links[k.Interface]
		switch k.Kind {
		case "mtu":
			l.MTU = v
		case "arp_ignore":
			f.arpIgnore[k.Interface] = v
		case "arp_announce":
			f.arpAnnounce[k.Interface] = v
		case "accept_ra":
			l.IPv6AcceptRA = &v
		case "autoconf":
			enabled := v != 0
			l.IPv6Autoconf = &enabled
		default:
			l.Up = v == 1
		}
		f.links[k.Interface] = l
	}
	return nil
}
func (f *fakeKernel) Firewall() (FirewallState, error) { return f.firewall, nil }
func (f *fakeKernel) ReplaceFirewall(v *Firewall) error {
	if err := f.op("firewall"); err != nil {
		return err
	}
	f.firewall = FirewallState{Table: FirewallTable, Present: v != nil, Owned: v != nil}
	return nil
}

func engineFor(t *testing.T, k *fakeKernel) (*Engine, Options) {
	t.Helper()
	opts := Options{StatePath: filepath.Join(t.TempDir(), "ownership.json")}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, opts
}
func yes() *bool { v := true; return &v }
func exampleDesired() Desired {
	return Desired{Interfaces: []Interface{{Name: "lan0", MTU: 1400, Addresses: []string{"192.0.2.1/24", "2001:db8:1::1/64"}}, {Name: "wan0", Addresses: []string{"198.51.100.2/24"}}}, Routes: []Route{{Destination: "0.0.0.0/0", Gateway: "198.51.100.1", Interface: "wan0", Metric: 100}}, Forwarding: Forwarding{IPv4: yes(), IPv6: yes()}, Firewall: &Firewall{Forward: []Forward{{From: "lan0", To: "wan0"}}, Masquerade: []Masquerade{{Source: "192.0.2.0/24", OutInterface: "wan0"}}}}
}

func TestRestartReconcileAndRollbackPreserveForeignState(t *testing.T) {
	k := fake()
	l := k.links["lan0"]
	l.Addresses = []AddressState{{Address: "203.0.113.7/32"}}
	k.links["lan0"] = l
	foreign := RouteState{Route: Route{Destination: "203.0.113.0/24", Interface: "lan0", Table: 254, Metric: 99}, Protocol: 4, Type: 1}
	k.routes = append(k.routes, foreign)
	e, opts := engineFor(t, k)
	desired := exampleDesired()
	s, err := e.Apply(context.Background(), 7, desired)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "applied" || s.Revision != 7 {
		t.Fatalf("snapshot=%+v", s)
	}
	desired.Interfaces[0].Addresses[0] = "192.0.2.99/24"
	if e.state.Desired.Interfaces[0].Addresses[0] != "192.0.2.1/24" {
		t.Fatal("caller mutated engine state")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	k.writes = nil
	if _, err := e.Apply(context.Background(), 7, exampleDesired()); err != nil {
		t.Fatal(err)
	}
	for _, w := range k.writes {
		if w == "address-add" || w == "route-add" {
			t.Fatalf("duplicate mutation after restart: %s", w)
		}
	}
	if _, err := e.Apply(context.Background(), 6, Desired{}); err != nil {
		t.Fatal(err)
	}
	l = k.links["lan0"]
	if l.Up || l.MTU != 1500 || len(l.Addresses) != 1 || l.Addresses[0].Address != "203.0.113.7/32" {
		t.Fatalf("original/foreign link state not restored: %+v", l)
	}
	if !reflect.DeepEqual(k.routes, []RouteState{foreign}) {
		t.Fatalf("foreign route changed: %+v", k.routes)
	}
	if k.forward4 != 0 || k.forward6 != 0 || k.firewall.Present {
		t.Fatalf("rollback left forwarding/firewall: %+v", k)
	}
}

func TestFailedCandidateSurvivesRestartAndRestoresBeforeImages(t *testing.T) {
	k := fake()
	k.fail = "setting-forwarding6"
	e, opts := engineFor(t, k)
	s, err := e.Apply(context.Background(), 2, exampleDesired())
	var ae *ApplyError
	if !errors.As(err, &ae) || !ae.Partial || s.Status != "degraded" || s.Revision != 0 {
		t.Fatalf("snapshot=%+v err=%v", s, err)
	}
	if !k.firewall.Present || k.forward4 != 1 {
		t.Fatal("test did not reach partial apply after firewall/IPv4")
	}
	_ = e.Close()
	k.fail = ""
	e, err = newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, Desired{}); err != nil {
		t.Fatal(err)
	}
	if k.firewall.Present || k.forward4 != 0 || k.forward6 != 0 || len(k.routes) != 0 {
		t.Fatal("partial candidate not reconciled away")
	}
	for _, l := range k.links {
		if l.Up || l.MTU != 1500 || len(l.Addresses) != 0 {
			t.Fatalf("before image not restored: %+v", l)
		}
	}
}

func TestUnownedCollisionIsNeverAdopted(t *testing.T) {
	for _, kind := range []string{"address", "route", "firewall"} {
		t.Run(kind, func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			d := exampleDesired()
			switch kind {
			case "address":
				l := k.links["lan0"]
				l.Addresses = []AddressState{{Address: "192.0.2.1/24"}}
				k.links["lan0"] = l
			case "route":
				r := d.Routes[0]
				r.Table = 254
				k.routes = []RouteState{{Route: r, Protocol: RouteProtocol, Type: 1}}
			case "firewall":
				k.firewall = FirewallState{Table: FirewallTable, Present: true, Owned: false}
			}
			if _, err := e.Apply(context.Background(), 1, d); err == nil || !strings.Contains(err.Error(), "ownership") {
				t.Fatalf("expected ownership conflict, got %v", err)
			}
			if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "address":
				if len(k.links["lan0"].Addresses) != 1 {
					t.Fatal("foreign address removed")
				}
			case "route":
				if len(k.routes) != 1 {
					t.Fatal("foreign route removed")
				}
			case "firewall":
				if !k.firewall.Present {
					t.Fatal("foreign firewall removed")
				}
			}
		})
	}
}

func TestDriftRestoredButForeignMTURestorationRefused(t *testing.T) {
	k := fake()
	e, _ := engineFor(t, k)
	d := Desired{Interfaces: []Interface{{Name: "lan0", MTU: 1400, Addresses: []string{"192.0.2.1/24"}}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	l := k.links["lan0"]
	l.Addresses = nil
	l.MTU = 1300
	k.links["lan0"] = l
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if k.links["lan0"].MTU != 1400 || len(k.links["lan0"].Addresses) != 1 {
		t.Fatal("managed drift not restored")
	}
	l = k.links["lan0"]
	l.MTU = 1200
	k.links["lan0"] = l
	if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
		t.Fatal("foreign MTU overwrite was not rejected")
	}
	if k.links["lan0"].MTU != 1200 {
		t.Fatal("foreign MTU overwritten")
	}
}

func TestJournalLock(t *testing.T) {
	k := fake()
	_, opts := engineFor(t, k)
	if e, err := newEngine(opts, k); err == nil {
		e.Close()
		t.Fatal("concurrent journal owner accepted")
	}
}

func TestValidateRejectsUnsafeOrAmbiguousIntent(t *testing.T) {
	for _, d := range []Desired{
		{Interfaces: []Interface{{Name: "../../proc"}}},
		{Interfaces: []Interface{{Name: "lan0", MTU: 1000, Addresses: []string{"2001:db8::1/64"}}}},
		{Routes: []Route{{Destination: "192.0.2.1/24", Interface: "lan0"}}},
		{Routes: []Route{{Destination: "::/0", Gateway: "192.0.2.1", Interface: "lan0"}}},
		{Routes: []Route{{Destination: "2001:0db8::/32", Interface: "lan0"}, {Destination: "2001:db8::/32", Interface: "wan0"}}},
		{Firewall: &Firewall{Masquerade: []Masquerade{{Source: "192.0.2.1/24", OutInterface: "wan0"}}}},
		{Links: []Link{{Name: "t0", Kind: "ip6tnl", Parent: "wan0", Local: "192.0.2.1", Remote: "2001:db8::2"}}},
	} {
		if err := Validate(d); err == nil {
			t.Fatalf("accepted invalid intent %+v", d)
		}
	}
	if err := Validate(exampleDesired()); err != nil {
		t.Fatal(err)
	}
}
