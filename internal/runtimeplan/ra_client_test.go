package runtimeplan

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func raClientFixture(now time.Time) (config.Native, raclient.Snapshot) {
	doc := config.Native{
		Services: []serviceconfig.Service{{Name: "wan-ra", RAClient: &serviceconfig.RAClient{Interface: "wan0", SourceAddress: "fe80::1", InterfaceID: "::1234"}}},
	}
	s := raclient.Snapshot{Resource: "wan-ra", Interface: "wan0", SourceAddress: "fe80::1", InterfaceID: "::1234", Active: true, UpdatedAt: now,
		Addresses: []raclient.Address{{Address: "2001:db8:1::1234/128", Prefix: "2001:db8:1::/64", PreferredUntil: now.Add(time.Hour), ValidUntil: now.Add(2 * time.Hour)}},
		Prefixes:  []raclient.Prefix{{Prefix: "2001:db8:1::/64", ValidUntil: now.Add(time.Hour)}},
		Routes: []raclient.Route{
			{Destination: "::/0", Gateway: "fe80::2", Preference: 1, ValidUntil: now.Add(10 * time.Second)},
			{Destination: "::/0", Gateway: "fe80::3", Preference: 1, ValidUntil: now.Add(30 * time.Second)},
			{Destination: "::/0", Gateway: "fe80::4", Preference: 0, ValidUntil: now.Add(40 * time.Second)},
			{Destination: "2001:db8:1::/64", Gateway: "fe80::2", Preference: 1, ValidUntil: now.Add(time.Hour)},
		},
	}
	return doc, s
}

func projectRAFixture(t *testing.T, doc config.Native, s raclient.Snapshot, now time.Time) projection {
	t.Helper()
	instance, err := doc.Services[0].Instance(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if err := validateSnapshot(instance, b); err != nil {
		t.Fatal(err)
	}
	p, err := projectNative(doc, "/run/routerd", "/var/lib/routerd", map[string]serviceconfig.Update{instance.Name: {Name: instance.Name, Role: "ra_client", Snapshot: b}}, now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRAClientProjectionOwnsSysctlsAndSeparateAddressAndRoutes(t *testing.T) {
	now := time.Now().UTC()
	doc, s := raClientFixture(now)
	initial, err := projectNative(doc, "/run/routerd", "/var/lib/routerd", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	iface := initial.network.Interfaces[0]
	if iface.IPv6AcceptRA == nil || *iface.IPv6AcceptRA != 0 || iface.IPv6Autoconf == nil || *iface.IPv6Autoconf || len(iface.Addresses) != 0 {
		t.Fatal("kernel RA ownership is not acquired before receiving any advertisement", iface)
	}
	p := projectRAFixture(t, doc, s, now)
	iface = p.network.Interfaces[0]
	options := iface.AddressOptions["2001:db8:1::1234/128"]
	if len(iface.Addresses) != 1 || iface.Addresses[0] != "2001:db8:1::1234/128" || options.NoDAD || !options.ValidUntil.Equal(s.Addresses[0].ValidUntil) {
		t.Fatal("SLAAC must use /128, normal DAD and the original finite lifetime", iface)
	}
	if len(p.network.Routes) != 2 {
		t.Fatal("on-link prefix should override an identical RIO, independently of A-bit address", p.network.Routes)
	}
	var def network.Route
	for _, r := range p.network.Routes {
		if r.Destination == "::/0" {
			def = r
		}
	}
	if len(def.NextHops) != 2 || def.NextHops[0].Gateway != "fe80::2" || def.NextHops[1].Gateway != "fe80::3" || !def.ValidUntil.Equal(s.Routes[0].ValidUntil) || def.Metric != 200 || def.Table != 254 {
		t.Fatal("highest preference routers require ECMP with the earliest member deadline", def)
	}
	for _, tc := range []struct {
		after time.Duration
		want  string
	}{{15 * time.Second, "fe80::3"}, {35 * time.Second, "fe80::4"}, {45 * time.Second, ""}} {
		p = projectRAFixture(t, doc, s, now.Add(tc.after))
		got := ""
		for _, route := range p.network.Routes {
			if route.Destination == "::/0" {
				got = route.Gateway
				if len(route.NextHops) != 0 {
					t.Fatal("expired ECMP member survived", route)
				}
			}
		}
		if got != tc.want {
			t.Fatalf("after %s gateway %q, want %q", tc.after, got, tc.want)
		}
	}
	s.Addresses[0].DADFailed = true
	p = projectRAFixture(t, doc, s, now)
	if len(p.network.Interfaces[0].Addresses) != 0 || len(p.network.Routes) != 2 {
		t.Fatal("DAD failure must withdraw only the SLAAC address", p.network)
	}
	s.Active = false
	p = projectRAFixture(t, doc, s, now)
	if len(p.network.Interfaces[0].Addresses) != 0 || len(p.network.Routes) != 0 {
		t.Fatal("inactive RA producer retained learned kernel state")
	}
}

func TestRAClientProjectionPreservesLongProtocolLifetimeWithRenewableKernelGrant(t *testing.T) {
	now := time.Now().UTC()
	doc, s := raClientFixture(now)
	deadline := now.Add(100 * 365 * 24 * time.Hour)
	s.Routes = []raclient.Route{{Destination: "2001:db8:ffff::/64", Gateway: "fe80::2", ValidUntil: deadline}}
	s.Prefixes = nil
	doc.Network.Links = []network.Link{{Name: "blue", Kind: "vrf", VRFTable: 123}}
	master := "blue"
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Master: &master}}
	p := projectRAFixture(t, doc, s, now)
	route := p.network.Routes[0]
	if route.Table != 123 || route.ValidUntil.Sub(now) < 6*24*time.Hour || route.ValidUntil.Sub(now) > 7*24*time.Hour || !s.Routes[0].ValidUntil.Equal(deadline) {
		t.Fatal("long RA lifetime was lost or lacks a bounded renewable kernel grant", route)
	}
	if !raKernelDeadline(deadline, now.Truncate(time.Hour)).Equal(route.ValidUntil) {
		t.Fatal("grant moved within the same UTC day")
	}
	next := raKernelDeadline(deadline, now.Add(24*time.Hour))
	if !next.Equal(route.ValidUntil.Add(24 * time.Hour)) {
		t.Fatal("long route grant is not renewed daily", next)
	}
	if !raKernelDeadline(time.Time{}, now).IsZero() {
		t.Fatal("explicit infinite lifetime became a finite protocol lifetime")
	}
}

func TestRAProjectionAndExpiryLeaveIntentAndEarlierProjectionUnchanged(t *testing.T) {
	now := time.Now().UTC()
	doc, snapshot := raClientFixture(now)
	expired := now.Add(-time.Second)
	addresses := make([]string, 2, 4)
	addresses[0], addresses[1] = "2001:db8:2::1/64", "2001:db8:3::1/64"
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Addresses: addresses, AddressOptions: map[string]network.AddressOptions{
		addresses[0]: {ValidUntil: expired, PreferredUntil: expired},
	}}}
	doc.Network.Routes = []network.Route{
		{Destination: "2001:db8:4::/64", Interface: "wan0", ValidUntil: expired},
		{Destination: "2001:db8:5::/64", Interface: "wan0"},
	}
	clock := boottime.Clock{WallTime: now}
	active := projectRAFixture(t, doc, snapshot, now)
	active.network = network.ProjectLifetimes(active.network, clock)
	if got := active.network.Interfaces[0].Addresses; len(got) != 2 || got[0] != addresses[1] || got[1] != snapshot.Addresses[0].Address {
		t.Fatal("expiry must retain static and learned addresses independently", got)
	}
	snapshot.Active = false
	inactive := projectRAFixture(t, doc, snapshot, now)
	inactive.network = network.ProjectLifetimes(inactive.network, clock)
	if got := inactive.network.Interfaces[0].Addresses; len(got) != 1 || got[0] != addresses[1] || len(inactive.network.Routes) != 1 {
		t.Fatal("withdrawal retained learned state or lost static state", inactive.network)
	}
	iface := doc.Network.Interfaces[0]
	if len(iface.Addresses) != 2 || iface.Addresses[0] != "2001:db8:2::1/64" || len(iface.AddressOptions) != 1 || iface.AddressOptions[iface.Addresses[0]].ValidUntil != expired || iface.IPv6AcceptRA != nil || iface.IPv6Autoconf != nil || doc.Network.Routes[0].Destination != "2001:db8:4::/64" {
		t.Fatal("projection or expiry wrote into operator intent", doc.Network)
	}
	if len(active.network.Interfaces[0].Addresses) != 2 || len(active.network.Interfaces[0].AddressOptions) != 1 || len(active.network.Routes) != 3 {
		t.Fatal("later withdrawal changed the earlier projection", active.network)
	}
}

func TestRAClientRejectsKernelCompetitionAndInvalidSnapshotIdentity(t *testing.T) {
	now := time.Now().UTC()
	doc, s := raClientFixture(now)
	on := 2
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", IPv6AcceptRA: &on}}
	if err := config.Validate(config.FromNative(doc)); err == nil {
		t.Fatal("competing kernel RA accepted")
	}
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8:1::1234/64"}}}
	b, _ := json.Marshal(s)
	if _, err := projectNative(doc, "/run/routerd", "/var/lib/routerd", map[string]serviceconfig.Update{"service.wan-ra": {Snapshot: b}}, now); err == nil {
		t.Fatal("SLAAC replaced an explicitly owned static address")
	}
	instance, err := doc.Services[0].Instance(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.InterfaceID = "::9999"
	b, _ = json.Marshal(s)
	if err := validateSnapshot(instance, b); err == nil {
		t.Fatal("foreign interface identity accepted")
	}
}
