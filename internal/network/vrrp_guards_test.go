//go:build linux

package network

import (
	"context"
	"testing"
)

func (f *fakeKernel) VRRPGuard() (FirewallState, error) {
	out := f.guard
	out.Table = VRRPGuardTable
	return out, nil
}
func (f *fakeKernel) ReplaceVRRPGuard(want []VRRPGuard) error {
	if err := f.op("vrrp-guard"); err != nil {
		return err
	}
	f.guard = FirewallState{Table: VRRPGuardTable, Present: len(want) > 0, Owned: len(want) > 0}
	return nil
}

func guardExample() Desired {
	return Desired{Interfaces: []Interface{{Name: "lan0", Addresses: []string{"192.0.2.1/24", "2001:db8:1::1/64"}}}, VRRPGuards: []VRRPGuard{{Family: 4, Interface: "lan0", Addresses: []string{"192.0.2.1"}}, {Family: 6, Interface: "lan0", Addresses: []string{"2001:db8:1::1"}}}}
}

func TestVRRPGuardIndependentOwnershipRestartAndRemoval(t *testing.T) {
	k := fake()
	k.firewall = FirewallState{Table: FirewallTable, Present: true, Owned: false}
	e, opts := engineFor(t, k)
	if _, err := e.Apply(context.Background(), 1, guardExample()); err != nil {
		t.Fatal(err)
	}
	if !k.guard.Present || !k.firewall.Present || k.firewall.Owned {
		t.Fatal("HA guard disturbed an unrelated forwarding table")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Apply(context.Background(), 1, guardExample()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if k.guard.Present || !k.firewall.Present || k.firewall.Owned {
		t.Fatal("HA guard did not clean up independently")
	}
}

func TestVRRPGuardRefusesUnownedTableBeforeVIPMutation(t *testing.T) {
	for _, markerPresent := range []bool{false, true} {
		k := fake()
		k.guard = FirewallState{Table: VRRPGuardTable, Present: true, Owned: markerPresent}
		e, _ := engineFor(t, k)
		if _, err := e.Apply(context.Background(), 1, guardExample()); err == nil {
			t.Fatal("adopted unowned HA table")
		}
		if len(k.writes) != 0 {
			t.Fatalf("changed kernel before ownership rejection: %v", k.writes)
		}
	}
}

func TestVRRPGuardValidationRejectsMissingVIPAndWrongFamily(t *testing.T) {
	for _, g := range []VRRPGuard{{Family: 4, Interface: "lan0", Addresses: []string{"192.0.2.2"}}, {Family: 6, Interface: "lan0", Addresses: []string{"192.0.2.1"}}, {Family: 4, Interface: "wan0", Addresses: []string{"192.0.2.1"}}, {Family: 4, Interface: "lan0", Addresses: []string{"192.0.2.1/24"}}} {
		d := guardExample()
		d.VRRPGuards = []VRRPGuard{g}
		if err := Validate(d); err == nil {
			t.Errorf("accepted invalid guard %+v", g)
		}
	}
}
