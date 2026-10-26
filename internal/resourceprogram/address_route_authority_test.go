package resourceprogram

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

func TestAddressAndRouteIntersectOriginalConditionsDependenciesAndUTCLifetimes(t *testing.T) {
	wall := time.Now().UTC()
	base := uint64(100 * time.Second)
	source := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "wan", ReadyWhen: &FactCondition{Fact: "ready"}, Interface: &network.Interface{Name: "wan0"}},
		{ID: "address", When: &FactCondition{Fact: "active", MaxAge: "600ms"}, Address: &Address{Interface: "wan0", CIDR: "2001:db8::2/64", Options: &network.AddressOptions{NoDAD: true, PreferredUntil: wall.Add(200 * time.Millisecond), ValidUntil: wall.Add(300 * time.Millisecond)}}},
		{ID: "route", DependsOn: []string{"address"}, Route: &network.Route{Destination: "2001:db8:1::/64", Gateway: "2001:db8::1", Interface: "wan0"}},
		{ID: "shorter-route", DependsOn: []string{"address"}, Route: &network.Route{Destination: "2001:db8:2::/64", Gateway: "2001:db8::1", Interface: "wan0", ValidUntil: lifetime.Deadline{UTC: wall.Add(250 * time.Millisecond)}}},
		{ID: "static", Address: &Address{Interface: "other0", CIDR: "192.0.2.2/24"}},
	}}
	before, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	facts := FactSnapshot{Scope: FactScope{BootID: "01234567-89ab-cdef-0123-456789abcdef"}, NowNanos: base, WallTime: wall, Facts: map[string]Fact{
		"active": {Value: True, ObservedNanos: base - uint64(100*time.Millisecond), ExpiresNanos: base + uint64(time.Second)},
		"ready":  {Value: True, ObservedNanos: base, ExpiresNanos: base + uint64(2*time.Second)},
	}}
	native, _ := plan.Project(facts)
	if err := network.Validate(native.Network); err != nil {
		t.Fatal(err)
	}
	until := base + uint64(300*time.Millisecond) // address lifetime is tighter than max_age
	for _, iface := range native.Network.Interfaces {
		if iface.Name == "other0" {
			if len(iface.AddressOptions) != 0 {
				t.Fatal("unconditional address acquired finite authority")
			}
			continue
		}
		options := iface.AddressOptions["2001:db8::2/64"]
		if !options.NoDAD || options.BootLifetime != (network.BootLifetime{BootID: facts.Scope.BootID, PreferredNanos: base + uint64(200*time.Millisecond), ValidNanos: until}) || !options.ValidUntil.IsZero() || !options.PreferredUntil.IsZero() {
			t.Fatal("address lost its tighter lifetime or original condition", options)
		}
	}
	if len(native.Network.Routes) != 2 || native.Network.Routes[0].ExpiresNanos != until || native.Network.Routes[1].ExpiresNanos != base+uint64(250*time.Millisecond) {
		t.Fatal("route lost its condition, dependency or shorter UTC deadline", native.Network.Routes)
	}
	for _, route := range native.Network.Routes {
		if route.BootID != facts.Scope.BootID {
			t.Fatal("route lost its original boot scope", route)
		}
	}
	if !native.Network.Routes[1].ValidUntil.UTC.Equal(wall.Add(250 * time.Millisecond)) {
		t.Fatal("projection discarded the explicit IPv6 kernel expiry", native.Network.Routes[1])
	}
	after, err := json.Marshal(source)
	if err != nil || string(before) != string(after) {
		t.Fatal("lifetime projection changed operator intent", err)
	}
	facts.NowNanos = until
	facts.WallTime = wall.Add(300 * time.Millisecond)
	native, states := plan.Project(facts)
	if states["address"].When != True || states["address"].Active || states["address"].Reason != "lifetime_expired" {
		t.Fatal("expired address remained ready while its fact was current", states)
	}
	if len(native.Network.Routes) != 0 {
		t.Fatal("expired address authority still released dependent routes")
	}
}
