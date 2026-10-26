package resourceprogram

import (
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
)

func TestConditionalLinkAndChildRetainOriginalFactDeadline(t *testing.T) {
	program := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "wan", When: &FactCondition{Fact: "service.ha.elected", MaxAge: "600ms"}, Link: &network.Link{Name: "wan-ha", Kind: "macvlan", Parent: "wan0", HardwareAddr: "02:00:5e:00:01:42"}},
		{ID: "vlan", Link: &network.Link{Name: "wan-vlan", Kind: "vlan", Parent: "wan-ha", VLANID: 42}},
		{ID: "static", Link: &network.Link{Name: "static", Kind: "bridge"}},
	}}
	plan, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(10 * time.Second)
	observed := now - uint64(100*time.Millisecond)
	facts := FactSnapshot{Scope: FactScope{BootID: "11111111-2222-4333-8444-555555555555"}, NowNanos: now, Facts: map[string]Fact{"service.ha.elected": {Value: True, ObservedNanos: observed, ExpiresNanos: now + uint64(time.Second)}}}
	native, states := plan.Project(facts)
	if !states["wan"].Active || !states["vlan"].Active {
		t.Fatal(states)
	}
	for _, link := range native.Network.Links {
		if link.Name == "static" {
			if link.Authority.BootID != "" || link.Authority.Nanos != 0 {
				t.Fatal("static link acquired a finite deadline")
			}
			continue
		}
		if link.Authority.BootID != facts.Scope.BootID || link.Authority.Nanos != observed+uint64(600*time.Millisecond)+1 {
			t.Fatal("link lost inherited original max-age boundary", link)
		}
	}
	facts.NowNanos = observed + uint64(600*time.Millisecond) + 1
	native, _ = plan.Project(facts)
	if len(native.Network.Links) != 1 || native.Network.Links[0].Name != "static" {
		t.Fatal("expired fact retained conditional links", native.Network.Links)
	}
}

func TestOperatorCannotDeclareInternalBootLinkAuthority(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		r := Resource{ID: "link", Link: &network.Link{Name: "wan-ha", Kind: "macvlan", Parent: "wan0", Authority: lifetime.Deadline{BootID: "11111111-2222-4333-8444-555555555555", Nanos: 10}}}
		if conditional {
			r.When = &FactCondition{Fact: "service.ha.elected"}
		}
		if _, err := Compile(Program{SchemaVersion: SchemaVersion, Resources: []Resource{r}}); err == nil {
			t.Fatal("operator supplied internal link authority")
		}
	}
}
