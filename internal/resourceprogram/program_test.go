package resourceprogram

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func pointer[T any](v T) *T { return &v }

func TestCompileGraphAndOwnershipIndependentOfResourceOrder(t *testing.T) {
	program := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "default", Owner: "wan", DependsOn: []string{"address"}, Route: &network.Route{Destination: "0.0.0.0/0", Interface: "wan0", Gateway: "192.0.2.1"}},
		{ID: "address", Owner: "wan", DependsOn: []string{"wan"}, Provenance: &Provenance{Source: "operator", Resource: "wan-address"}, Address: &Address{Interface: "wan0", CIDR: "192.0.2.2/24"}},
		{ID: "wan", Interface: &network.Interface{Name: "wan0", MTU: 1500}},
	}}
	plan, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Order, []string{"wan", "address", "default"}) {
		t.Fatalf("dependency order = %v", plan.Order)
	}
	if !slices.Equal(plan.Dependencies["default"], []string{"address", "wan"}) {
		t.Fatalf("explicit and typed dependency union = %v", plan.Dependencies)
	}
	if len(plan.Native.Network.Interfaces) != 1 || plan.Native.Network.Interfaces[0].MTU != 1500 || !slices.Equal(plan.Native.Network.Interfaces[0].Addresses, []string{"192.0.2.2/24"}) {
		t.Fatalf("address did not merge into declared interface: %+v", plan.Native.Network.Interfaces)
	}
	want := []Ownership{
		{ResourceID: "wan", OwnerID: "wan", Kind: "interface", Key: "wan0"},
		{ResourceID: "address", OwnerID: "wan", Kind: "address", Key: "wan0/192.0.2.2/24", Provenance: &Provenance{Source: "operator", Resource: "wan-address"}},
		{ResourceID: "default", OwnerID: "wan", Kind: "route", Key: "254/0.0.0.0/0/0"},
	}
	if !reflect.DeepEqual(plan.Ownership, want) {
		t.Fatalf("ownership = %#v, want %#v", plan.Ownership, want)
	}
	slices.Reverse(program.Resources)
	reordered, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, reordered) {
		t.Fatal("moving YAML declarations changed the execution plan")
	}
}

func TestRejectInvalidProgramBeforeReturningProjection(t *testing.T) {
	base := Resource{ID: "lan", Interface: &network.Interface{Name: "lan0"}}
	cases := []struct {
		name    string
		program Program
		message string
	}{
		{"old schema", Program{SchemaVersion: 1}, "unsupported config schema"},
		{"missing ID", Program{SchemaVersion: 2, Resources: []Resource{{Interface: base.Interface}}}, "invalid resource id"},
		{"duplicate ID", Program{SchemaVersion: 2, Resources: []Resource{base, base}}, "duplicate resource id"},
		{"no payload", Program{SchemaVersion: 2, Resources: []Resource{{ID: "empty"}}}, "exactly one"},
		{"multiple payloads", Program{SchemaVersion: 2, Resources: []Resource{{ID: "ambiguous", Interface: base.Interface, Route: &network.Route{}}}}, "exactly one"},
		{"missing dependency", Program{SchemaVersion: 2, Resources: []Resource{{ID: "lan", DependsOn: []string{"absent"}, Interface: base.Interface}}}, "missing resource"},
		{"self dependency", Program{SchemaVersion: 2, Resources: []Resource{{ID: "lan", DependsOn: []string{"lan"}, Interface: base.Interface}}}, "self or duplicate"},
		{"duplicate dependency", Program{SchemaVersion: 2, Resources: []Resource{base, {ID: "addr", DependsOn: []string{"lan", "lan"}, Address: &Address{Interface: "lan0", CIDR: "192.0.2.1/24"}}}}, "self or duplicate"},
		{"missing owner", Program{SchemaVersion: 2, Resources: []Resource{{ID: "lan", Owner: "absent", Interface: base.Interface}}}, "missing owner"},
		{"dependency cycle", Program{SchemaVersion: 2, Resources: []Resource{{ID: "a", DependsOn: []string{"b"}, Interface: base.Interface}, {ID: "b", DependsOn: []string{"a"}, Interface: &network.Interface{Name: "wan0"}}}}, "cycle"},
		{"owner cycle", Program{SchemaVersion: 2, Resources: []Resource{{ID: "a", Owner: "b", Interface: base.Interface}, {ID: "b", Owner: "a", Interface: &network.Interface{Name: "wan0"}}}}, "cycle"},
		{"mixed cycle", Program{SchemaVersion: 2, Resources: []Resource{{ID: "a", Owner: "b", Interface: base.Interface}, {ID: "b", DependsOn: []string{"a"}, Interface: &network.Interface{Name: "wan0"}}}}, "cycle"},
		{"embedded address", Program{SchemaVersion: 2, Resources: []Resource{{ID: "lan", Interface: &network.Interface{Name: "lan0", Addresses: []string{"192.0.2.1/24"}}}}}, "separate address resources"},
		{"empty forwarding", Program{SchemaVersion: 2, Resources: []Resource{{ID: "forwarding", Forwarding: &network.Forwarding{}}}}, "requires at least one setting"},
		{"invalid provenance", Program{SchemaVersion: 2, Resources: []Resource{{ID: "lan", Provenance: &Provenance{Path: "a\nb"}, Interface: base.Interface}}}, "provenance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Compile(tc.program)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("Compile error = %v, want %q", err, tc.message)
			}
			if !reflect.DeepEqual(plan, Plan{}) {
				t.Fatalf("failed compile returned partial executable plan: %+v", plan)
			}
		})
	}
}

func TestRejectDuplicateOutputOwnership(t *testing.T) {
	cases := []struct {
		name      string
		resources []Resource
	}{
		{"interface", []Resource{{ID: "a", Interface: &network.Interface{Name: "eth0"}}, {ID: "b", Interface: &network.Interface{Name: "eth0", MTU: 1400}}}},
		{"normalized address", []Resource{{ID: "a", Address: &Address{Interface: "eth0", CIDR: "2001:db8::1/64"}}, {ID: "b", Address: &Address{Interface: "eth0", CIDR: "2001:0db8:0000::1/64"}}}},
		{"route default table", []Resource{{ID: "a", Route: &network.Route{Destination: "0.0.0.0/0", Interface: "eth0"}}, {ID: "b", Route: &network.Route{Destination: "0.0.0.0/0", Interface: "eth1", Table: 254}}}},
		{"IPv6 default metric", []Resource{{ID: "a", Route: &network.Route{Destination: "::/0", Interface: "eth0"}}, {ID: "b", Route: &network.Route{Destination: "::/0", Interface: "eth1", Metric: 1024}}}},
		{"policy slot", []Resource{{ID: "a", PolicyRule: &network.PolicyRule{Family: 4, Priority: 100, Table: 100}}, {ID: "b", PolicyRule: &network.PolicyRule{Family: 4, Priority: 100, Table: 200}}}},
		{"forwarding family", []Resource{{ID: "a", Forwarding: &network.Forwarding{IPv4: pointer(true)}}, {ID: "b", Forwarding: &network.Forwarding{IPv4: pointer(false)}}}},
		{"firewall table", []Resource{{ID: "a", Firewall: &network.Firewall{}}, {ID: "b", Firewall: &network.Firewall{}}}},
		{"link namespace", []Resource{{ID: "a", Link: &network.Link{Name: "b4", Kind: "bridge"}}, {ID: "b", Link: &network.Link{Name: "b4", Kind: "ip6tnl", Parent: "eth0", Local: "2001:db8::1", Remote: "2001:db8::2"}}}},
		{"service instance", []Resource{{ID: "a", Service: &serviceconfig.Service{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "eth0"}}}, {ID: "b", Service: &serviceconfig.Service{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "eth1"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(Program{SchemaVersion: SchemaVersion, Resources: tc.resources})
			if err == nil || !strings.Contains(err.Error(), "both own") {
				t.Fatalf("duplicate owners accepted: %v", err)
			}
		})
	}
}

func TestAddressOwnersRemainIndependent(t *testing.T) {
	program := Program{SchemaVersion: SchemaVersion, Resources: []Resource{
		{ID: "first", Provenance: &Provenance{Source: "manual"}, Address: &Address{Interface: "eth0", CIDR: "192.0.2.1/24"}},
		{ID: "second", Address: &Address{Interface: "eth0", CIDR: "2001:db8::1/64", Options: &network.AddressOptions{NoDAD: true}}},
		{ID: "forward4", Forwarding: &network.Forwarding{IPv4: pointer(true)}},
		{ID: "forward6", Forwarding: &network.Forwarding{IPv6: pointer(true)}},
	}}
	if _, err := Compile(program); err != nil {
		t.Fatal(err)
	}
	program.Resources = program.Resources[1:]
	remaining, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	if got := remaining.Native.Network.Interfaces[0].Addresses; !slices.Equal(got, []string{"2001:db8::1/64"}) {
		t.Fatalf("removing one address owner changed another owner: %v", got)
	}
	if !remaining.Native.Network.Interfaces[0].AddressOptions["2001:db8::1/64"].NoDAD {
		t.Fatal("remaining address lost its options")
	}
	for _, owner := range remaining.Ownership {
		if owner.OwnerID == "first" {
			t.Fatal("removed owner retained an output")
		}
	}
}

func TestFromNativePreservesEverySupportedProjection(t *testing.T) {
	native := Native{
		Network: network.Desired{
			Links: []network.Link{{Name: "wg0", Kind: "wireguard"}, {Name: "b4", Kind: "ip6tnl", Parent: "wan0", Local: "2001:db8::1", Remote: "2001:db8::ffff"}, {Name: "br0", Kind: "bridge", BridgeMulticastSnooping: pointer(false)}},
			Interfaces: []network.Interface{
				{Name: "wan0", MTU: 1500, Addresses: []string{"192.0.2.2/24", "192.0.2.3/24", "2001:db8::1/64"}, AddressOptions: map[string]network.AddressOptions{"2001:db8::1/64": {NoDAD: true}}, IPv6AcceptRA: pointer(0), IPv6Autoconf: pointer(false)},
				{Name: "wg0", MTU: 1380, Addresses: []string{"10.0.0.1/24"}},
				{Name: "b4", MTU: 1460, Addresses: []string{"192.0.0.2/29"}},
				{Name: "lan0", Master: pointer("br0"), Alias: pointer("LAN")},
			},
			Routes:      []network.Route{{Destination: "0.0.0.0/0", Interface: "b4", Table: 100}, {Destination: "2001:db8:1::/64", NextHops: []network.NextHop{{Gateway: "2001:db8::2", Interface: "wan0", Weight: 1}, {Gateway: "2001:db8::3", Interface: "wan0", Weight: 2}}, Metric: 50}},
			PolicyRules: []network.PolicyRule{{Family: 4, Priority: 100, Table: 100, Source: "10.0.0.0/24", Mark: pointer(uint32(1)), Mask: pointer(uint32(255))}},
			Forwarding:  network.Forwarding{IPv4: pointer(true), IPv6: pointer(false)},
			Firewall:    &network.Firewall{Forward: []network.Forward{{From: "lan0", To: "b4"}}, NAT: []network.NATRule{{Type: "masquerade", Match: network.PacketMatch{Family: 4, Source: "10.0.0.0/24", OutInterface: "b4"}}}},
			VRRPGuards:  []network.VRRPGuard{{Family: 4, Interface: "wan0", Addresses: []string{"192.0.2.3"}}},
			WireGuard:   []network.WireGuard{{Interface: "wg0", PrivateKeyFile: "/var/lib/routerd/keys/wg0", GeneratePrivateKey: true, ListenPort: 51820, FirewallMark: 200}},
		},
		Services: []serviceconfig.Service{{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0", Hostname: "edge", Metric: 100}}},
	}
	program := FromNative(native)
	plan, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstructing from the compiler's output must retain every field,
	// including nested protocol intent, ordered firewall data and address options.
	before, _ := json.Marshal(program)
	after, _ := json.Marshal(FromNative(plan.Native))
	if string(before) != string(after) {
		t.Fatalf("native round trip lost or changed intent:\nbefore %s\nafter  %s", before, after)
	}
	if len(plan.Native.Network.Links) != 3 || len(plan.Native.Network.Interfaces) != 4 || len(plan.Native.Network.Routes) != 2 || len(plan.Native.Network.PolicyRules) != 1 || plan.Native.Network.Firewall == nil || len(plan.Native.Network.VRRPGuards) != 1 || len(plan.Native.Network.WireGuard) != 1 || len(plan.Native.Services) != 1 {
		t.Fatal("one or more supported native roles were omitted")
	}
	var wan network.Interface
	for _, iface := range plan.Native.Network.Interfaces {
		if iface.Name == "wan0" {
			wan = iface
		}
	}
	if !slices.Equal(wan.Addresses, native.Network.Interfaces[0].Addresses) {
		t.Fatalf("primary address order changed: %v", wan.Addresses)
	}
}

func TestFromNativeCannotSilentlyRepairInvalidAddressOptions(t *testing.T) {
	for _, options := range []map[string]network.AddressOptions{
		{"2001:db8::2/64": {NoDAD: true}},
		{"2001:db8::1/64": {NoDAD: true}, "2001:0db8::1/64": {}},
	} {
		native := Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: options}}}}
		if _, err := Compile(FromNative(native)); err == nil {
			t.Fatal("invalid address options were silently dropped or adopted")
		}
	}
}

func TestStableIDsDoNotDependOnMutableRouteNextHop(t *testing.T) {
	native := Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0"}}, Routes: []network.Route{{Destination: "0.0.0.0/0", Interface: "eth0", Gateway: "192.0.2.1"}}}}
	first := FromNative(native)
	native.Network.Routes[0].Gateway = "192.0.2.2"
	second := FromNative(native)
	if len(first.Resources) != len(second.Resources) {
		t.Fatal("changed resource count")
	}
	for i := range first.Resources {
		if first.Resources[i].ID != second.Resources[i].ID {
			t.Fatal("next-hop update replaced source identity")
		}
	}
	if first.Resources[0].ID != "interface/eth0" {
		t.Fatalf("interface ID should be readable: %s", first.Resources[0].ID)
	}
}

func TestLargeRouteProgram(t *testing.T) {
	native := Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0"}}}}
	for i := range 8192 {
		native.Network.Routes = append(native.Network.Routes, network.Route{Destination: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Interface: "eth0"})
	}
	program := FromNative(native)
	plan, err := Compile(program)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Native.Network.Routes) != 8192 || len(plan.Ownership) != 8193 || len(plan.Order) != 8193 || plan.Order[0] != "interface/eth0" {
		t.Fatal("large source program lost resources or dependencies")
	}
}

func TestCycleDiagnosticIsBounded(t *testing.T) {
	graph := map[string][]string{}
	for i := range 1000 {
		graph[fmt.Sprintf("node-%04d", i)] = []string{fmt.Sprintf("node-%04d", (i+1)%1000)}
	}
	_, err := dependencyOrder(graph)
	if err == nil || !strings.Contains(err.Error(), "984 more resources") || len(err.Error()) > 1024 {
		t.Fatalf("unbounded or missing cycle diagnostic: %v", err)
	}
}
