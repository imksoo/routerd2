//go:build linux

package network

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func (f *fakeKernel) PolicyRules() ([]PolicyRuleState, error) {
	return append([]PolicyRuleState(nil), f.rules...), nil
}
func (f *fakeKernel) AddPolicyRule(r PolicyRule) error {
	if err := f.op("rule-add"); err != nil {
		return err
	}
	f.rules = append(f.rules, PolicyRuleState{PolicyRule: r, Protocol: RouteProtocol, Action: r.action()})
	return nil
}
func (f *fakeKernel) DeletePolicyRule(r PolicyRule) error {
	if err := f.op("rule-delete"); err != nil {
		return err
	}
	for i, v := range f.rules {
		if policyRuleMatches(v, r) {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return errors.New("missing policy rule")
}
func uint32Pointer(v uint32) *uint32 { return &v }

func TestPolicyRuleRestartUpdateRemovePreservesForeignRules(t *testing.T) {
	k := fake()
	foreign := PolicyRuleState{PolicyRule: PolicyRule{Family: 4, Priority: 9000, Table: 200}, Protocol: 4, Action: 1}
	k.rules = []PolicyRuleState{foreign}
	e, opts := engineFor(t, k)
	d := Desired{PolicyRules: []PolicyRule{{Family: 4, Priority: 12000, Table: 100, Source: "192.0.2.0/24", InInterface: "lan0", Mark: uint32Pointer(16), Mask: uint32Pointer(255)}, {Family: 6, Priority: 12000, Table: 101, Destination: "2001:db8::/32"}, {Family: 4, Priority: NoPathPriority, Blackhole: true, Mark: uint32Pointer(NoPathMark)}}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
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
	if len(k.writes) != 0 {
		t.Fatalf("repeat policy writes: %v", k.writes)
	}
	d.PolicyRules[0].Table = 102
	if _, err := e.Apply(context.Background(), 2, d); err != nil {
		t.Fatal(err)
	}
	if len(k.rules) != 4 {
		t.Fatalf("duplicate rule after update: %+v", k.rules)
	}
	if _, err := e.Apply(context.Background(), 0, Desired{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(k.rules, []PolicyRuleState{foreign}) {
		t.Fatalf("foreign rule changed: %+v", k.rules)
	}
}

func TestPolicyRuleRefusesCollisionOrChangedAction(t *testing.T) {
	for _, scenario := range []string{"foreign", "action", "selector", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			k := fake()
			e, _ := engineFor(t, k)
			r := PolicyRule{Family: 4, Priority: 12000, Table: 100}
			if scenario == "foreign" {
				k.rules = []PolicyRuleState{{PolicyRule: r, Protocol: 4, Action: 1}}
			} else {
				if _, err := e.Apply(context.Background(), 1, Desired{PolicyRules: []PolicyRule{r}}); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "action":
					k.rules[0].Action = 6
				case "selector":
					k.rules[0].Advanced = true
				case "duplicate":
					k.rules = append(k.rules, k.rules[0])
				}
			}
			before := append([]PolicyRuleState(nil), k.rules...)
			k.writes = nil
			if _, err := e.Apply(context.Background(), 2, Desired{PolicyRules: []PolicyRule{r}}); err == nil {
				t.Fatal("accepted ambiguous/foreign rule")
			}
			if len(k.writes) != 0 {
				t.Fatalf("mutation before refusing rule: %v", k.writes)
			}
			if scenario != "foreign" {
				if _, err := e.Apply(context.Background(), 0, Desired{}); err == nil {
					t.Fatal("removed changed rule")
				}
			}
			if !reflect.DeepEqual(k.rules, before) {
				t.Fatal("foreign policy rule was modified")
			}
		})
	}
}

func TestPolicyRuleValidation(t *testing.T) {
	for _, r := range []PolicyRule{
		{Family: 0, Priority: 12000, Table: 100}, {Family: 4, Priority: 0, Table: 100}, {Family: 4, Priority: 32766, Table: 100},
		{Family: 4, Priority: 12000, Table: 255}, {Family: 4, Priority: 12000, Table: 100, Source: "2001:db8::/32"},
		{Family: 4, Priority: 12000, Table: 100, Source: "192.0.2.1/24"}, {Family: 4, Priority: 12000, Table: 100, Mask: uint32Pointer(255)},
		{Family: 4, Priority: 12000, Table: 100, Mark: uint32Pointer(256), Mask: uint32Pointer(255)},
	} {
		if err := Validate(Desired{PolicyRules: []PolicyRule{r}}); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	r := PolicyRule{Family: 4, Priority: 12000, Table: 100, Source: "0.0.0.0/0", Mark: uint32Pointer(0)}
	n := normalizePolicyRule(r)
	if n.Source != "" || n.Mask == nil || *n.Mask != ^uint32(0) {
		t.Fatalf("unexpected normalization: %+v", n)
	}
}

func TestPolicyRuleDecoderRetainsActionAndUnknownSelectors(t *testing.T) {
	message := nl.NewRtMsg()
	message.Family = unix.AF_INET
	message.Type = 6
	message.Table = 100
	raw := append([]byte(nil), message.Serialize()...)
	for _, attr := range []*nl.RtAttr{nl.NewRtAttr(nl.FRA_PRIORITY, nl.Uint32Attr(12000)), nl.NewRtAttr(nl.FRA_PROTOCOL, nl.Uint8Attr(RouteProtocol)), nl.NewRtAttr(nl.FRA_L3MDEV, nl.Uint8Attr(1))} {
		raw = append(raw, attr.Serialize()...)
	}
	r, err := decodeNativePolicyRule(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Action != 6 || !r.Advanced || r.Priority != 12000 || r.Table != 100 || r.Protocol != RouteProtocol {
		t.Fatalf("lost ownership-critical fields: %+v", r)
	}
	if policyRuleMatches(r, PolicyRule{Family: 4, Priority: 12000, Table: 100}) {
		t.Fatal("foreign blackhole matched a to-table rule")
	}
	if _, err := decodeNativePolicyRule([]byte{1, 2}); err == nil {
		t.Fatal("accepted truncated header")
	}
}
