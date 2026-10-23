//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func (f *fakeKernel) Interfaces() ([]InterfaceState, error) {
	var out []InterfaceState
	for _, l := range f.links {
		out = append(out, l)
	}
	return out, nil
}
func (f *fakeKernel) CreateLink(d Link, alias string) error {
	if err := f.op("link-create"); err != nil {
		return err
	}
	if _, ok := f.links[d.Name]; ok {
		return errors.New("link exists")
	}
	index := 1
	for _, l := range f.links {
		if l.Index >= index {
			index = l.Index + 1
		}
	}
	if d.Parent != "" {
		if _, ok := f.links[d.Parent]; !ok {
			return errors.New("missing parent")
		}
	}
	if d.HardwareAddr == "" && d.Kind != "vrf" {
		d.HardwareAddr = fmt.Sprintf("02:00:00:00:00:%02x", index)
	}
	f.links[d.Name] = InterfaceState{Name: d.Name, Index: index, Kind: d.Kind, MTU: 1500, Alias: alias, HardwareAddr: d.HardwareAddr, Parent: d.Parent, Link: &d}
	return nil
}
func (f *fakeKernel) UpdateLink(d Link, alias string) error {
	if err := f.op("link-update"); err != nil {
		return err
	}
	l, ok := f.links[d.Name]
	if !ok {
		return errLinkNotFound
	}
	if d.HardwareAddr == "" {
		d.HardwareAddr = l.HardwareAddr
	} else {
		l.HardwareAddr = d.HardwareAddr
	}
	l.Link = &d
	l.Alias = alias
	f.links[d.Name] = l
	return nil
}
func (f *fakeKernel) DeleteLink(name string) error {
	if err := f.op("link-delete"); err != nil {
		return err
	}
	if _, ok := f.links[name]; !ok {
		return errLinkNotFound
	}
	delete(f.links, name)
	return nil
}
func (f *fakeKernel) WriteAttribute(key settingKey, value string) error {
	if err := f.op("attribute-" + key.Kind); err != nil {
		return err
	}
	l, ok := f.links[key.Interface]
	if !ok {
		return errLinkNotFound
	}
	switch key.Kind {
	case "alias":
		l.Alias = value
	case "master":
		l.Master = value
		l.MasterIndex = 0
		if value != "" {
			m, ok := f.links[value]
			if !ok {
				return errLinkNotFound
			}
			l.MasterIndex = m.Index
		}
	default:
		return errors.New("unknown attribute")
	}
	f.links[key.Interface] = l
	return nil
}

func stringPointer(s string) *string { return &s }
func linkExample() Desired {
	return Desired{Links: []Link{
		{Name: "vlan12", Kind: "vlan", Parent: "br0", VLANID: 12},
		{Name: "br0", Kind: "bridge", Alias: "customers"},
		{Name: "vrf-blue", Kind: "vrf", VRFTable: 100},
		{Name: "vxlan42", Kind: "vxlan", Parent: "wan0", VXLANID: 42, Local: "198.51.100.2", Remote: "198.51.100.3"},
		{Name: "vrrp4", Kind: "macvlan", Parent: "br0", HardwareAddr: "00:00:5e:00:01:2a", MACVLANMode: "bridge"},
	}, Interfaces: []Interface{
		{Name: "br0"}, {Name: "vlan12", Addresses: []string{"192.0.2.1/24"}, Master: stringPointer("vrf-blue")},
		{Name: "vrf-blue"}, {Name: "vxlan42", Master: stringPointer("br0")}, {Name: "vrrp4", Addresses: []string{"203.0.113.1/24"}},
		{Name: "lan0", Master: stringPointer("br0"), Alias: stringPointer("member")},
	}}
}

func TestLinksTopologyRestartUpdateAndRestoreOnlyTouchedFields(t *testing.T) {
	k := fake()
	before := k.links["lan0"]
	before.Alias = "external"
	k.links["lan0"] = before
	e, opts := engineFor(t, k)
	d := linkExample()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if k.links["vlan12"].Index < k.links["br0"].Index {
		t.Fatal("child was created before parent")
	}
	if k.links["lan0"].Master != "br0" || k.links["vlan12"].Master != "vrf-blue" {
		t.Fatal("membership not installed")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	k.writes = nil
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	for _, w := range k.writes {
		if w == "link-create" || w == "link-update" {
			t.Fatalf("unexpected repeat mutation: %s", w)
		}
	}
	d.Links[1].Alias = "renamed"
	d.Links[1].BridgeMulticastSnooping = boolPointer(false)
	d.Links[3].Remote = "198.51.100.4"
	d.Links[3].Learning = boolPointer(false)
	if _, err := e.Apply(context.Background(), 2, d); err != nil {
		t.Fatal(err)
	}
	if k.links["br0"].Link.Alias != "renamed" || *k.links["br0"].Link.BridgeMulticastSnooping || k.links["vxlan42"].Link.Remote != "198.51.100.4" {
		t.Fatal("mutable link fields not updated")
	}
	// MTU was never requested on the existing port; preserve an unrelated edit.
	port := k.links["lan0"]
	port.MTU = 1492
	k.links["lan0"] = port
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if len(k.links) != 2 {
		t.Fatalf("virtual links remain: %+v", k.links)
	}
	port = k.links["lan0"]
	if port.Master != "" || port.MasterIndex != 0 || port.Alias != "external" || port.Up || port.MTU != 1492 {
		t.Fatalf("preexisting port state not restored selectively: %+v", port)
	}
	if len(e.state.Links) != 0 || len(e.state.Attributes) != 0 {
		t.Fatal("ownership records remain")
	}
}

func TestLinksRejectForeignOwnershipAndIdentityDriftBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"unowned", "marker", "replacement", "immutable", "managed-drift"} {
		t.Run(scenario, func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			d := Desired{Links: []Link{{Name: "br0", Kind: "bridge"}}}
			if scenario == "unowned" {
				k.links["br0"] = InterfaceState{Name: "br0", Index: 3, Kind: "bridge"}
			} else {
				if _, err := e.Apply(context.Background(), 1, d); err != nil {
					t.Fatal(err)
				}
				l := k.links["br0"]
				switch scenario {
				case "marker":
					l.Alias = "foreign"
				case "replacement":
					l.Index++
				case "immutable":
					d.Links[0].Kind = "vrf"
					d.Links[0].VRFTable = 100
				case "managed-drift":
					copy := *l.Link
					copy.BridgeVLANFiltering = boolPointer(true)
					l.Link = &copy
				}
				k.links["br0"] = l
			}
			k.writes = nil
			if _, err := e.Apply(context.Background(), 2, d); err == nil {
				t.Fatal("accepted foreign or incompatible link")
			}
			if len(k.writes) != 0 {
				t.Fatalf("mutation before rejecting ownership: %v", k.writes)
			}
		})
	}
}

func TestLinksDoNotDeleteForeignReferences(t *testing.T) {
	for _, scenario := range []string{"address", "route", "child", "member"} {
		t.Run(scenario, func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			d := Desired{Links: []Link{{Name: "br0", Kind: "bridge"}}}
			if _, err := e.Apply(context.Background(), 1, d); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "address":
				l := k.links["br0"]
				l.Addresses = []AddressState{{Address: "203.0.113.9/32"}}
				k.links["br0"] = l
			case "route":
				k.routes = []RouteState{{Route: Route{Destination: "203.0.113.0/24", Interface: "br0", Table: 254}, Protocol: 4, Type: 1}}
			case "child":
				k.links["foreign"] = InterfaceState{Name: "foreign", Index: 8, Kind: "vlan", Parent: "br0"}
			case "member":
				l := k.links["lan0"]
				l.Master = "br0"
				l.MasterIndex = k.links["br0"].Index
				k.links["lan0"] = l
			}
			if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
				t.Fatal("deleted a link with foreign references")
			}
			if _, ok := k.links["br0"]; !ok {
				t.Fatal("owned parent disappeared")
			}
		})
	}
}

func TestExistingAttributesRestoreMembershipAndRejectExternalChange(t *testing.T) {
	k := fake()
	k.links["oldbr"] = InterfaceState{Name: "oldbr", Index: 7, Kind: "bridge", MTU: 1500}
	l := k.links["lan0"]
	l.Master = "oldbr"
	l.MasterIndex = 7
	l.Alias = "before"
	k.links["lan0"] = l
	e, _ := engineFor(t, k)
	d := Desired{Links: []Link{{Name: "br0", Kind: "bridge"}}, Interfaces: []Interface{{Name: "lan0", Master: stringPointer("br0"), Alias: stringPointer("ours")}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	got := k.links["lan0"]
	if got.Master != "oldbr" || got.MasterIndex != 7 || got.Alias != "before" {
		t.Fatalf("original attributes lost: %+v", got)
	}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	got = k.links["lan0"]
	got.Alias = "changed externally"
	k.links["lan0"] = got
	if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
		t.Fatal("overwrote foreign alias")
	}
	if k.links["lan0"].Alias != "changed externally" {
		t.Fatal("foreign alias lost")
	}
}

func TestPartialLinkUpdateCanRollBackAfterRestart(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	d := Desired{Links: []Link{{Name: "br0", Kind: "bridge", Alias: "before"}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	// Model an interrupted native update after the first netlink operation.
	before := e.state.Links[0].Definition
	candidate := before
	candidate.Alias = "candidate"
	candidate.BridgeVLANFiltering = boolPointer(true)
	e.state.Links[0].Attempted = &candidate
	e.state.Status = "applying"
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	l := k.links["br0"]
	part := *l.Link
	part.BridgeVLANFiltering = boolPointer(true)
	l.Link = &part
	k.links["br0"] = l
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if !sameManagedLink(*k.links["br0"].Link, before) || e.state.Links[0].Attempted != nil {
		t.Fatal("interrupted definition not rolled back")
	}
}

func TestLinkValidationRejectsUnsupportedAndCyclicModels(t *testing.T) {
	cases := []Desired{
		{Links: []Link{{Name: "x", Kind: "dummy"}}},
		{Links: []Link{{Name: "x", Kind: "vlan", Parent: "lan0", VLANID: 0}}},
		{Links: []Link{{Name: "x", Kind: "bridge", VLANID: 12}}},
		{Links: []Link{{Name: "x", Kind: "vrf", VRFTable: 254}}},
		{Links: []Link{{Name: "x", Kind: "macvlan", Parent: "lan0", MACVLANMode: "passthru"}}},
		{Links: []Link{{Name: "x", Kind: "macvlan", Parent: "lan0", HardwareAddr: "01:00:5e:00:00:01"}}},
		{Links: []Link{{Name: "x", Kind: "vxlan", Parent: "wan0", VXLANID: 42, Remote: "192.0.2.1", Group: "239.1.1.1"}}},
		{Links: []Link{{Name: "a", Kind: "vlan", Parent: "b", VLANID: 1}, {Name: "b", Kind: "vlan", Parent: "a", VLANID: 2}}},
		{Links: []Link{{Name: "a", Kind: "bridge"}, {Name: "b", Kind: "bridge"}}, Interfaces: []Interface{{Name: "a", Master: stringPointer("b")}, {Name: "b", Master: stringPointer("a")}}},
		{Links: []Link{{Name: "x", Kind: "bridge"}}, Interfaces: []Interface{{Name: "x", Alias: stringPointer("erase marker")}}},
	}
	for i, d := range cases {
		if err := Validate(d); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	valid := linkExample()
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized(cloneDesired(valid)), normalized(normalized(cloneDesired(valid)))) {
		t.Fatal("normalization is not idempotent")
	}
	if err := Validate(Desired{Links: []Link{{Name: "vrrp6", Kind: "macvlan", Parent: "lan0", HardwareAddr: "00:00:5e:00:02:ff", MACVLANMode: "private"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(automaticLinkLocal(InterfaceState{HardwareAddr: "00:00:5e:00:01:2a"}), "fe80::200:5eff:fe00:12a/") {
		t.Fatal("unexpected EUI-64 link-local")
	}
}
