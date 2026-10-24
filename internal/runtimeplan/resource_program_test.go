package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// Keep this fixture in source form. The protocol projection tests use Native
// builders, but these tests exercise the actual ResourceProgram entry point.
func sourceProgram(address, gateway string) config.Document {
	return config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "lan-route", Owner: "lan", DependsOn: []string{"lan-address"}, Route: &network.Route{Destination: "198.51.100.0/24", Interface: "lan0", Gateway: gateway}},
		{ID: "unrelated-address", Owner: "unrelated", DependsOn: []string{"unrelated"}, Address: &resourceprogram.Address{Interface: "other0", CIDR: "203.0.113.1/24"}},
		{ID: "lan-address", Owner: "lan", DependsOn: []string{"lan"}, Address: &resourceprogram.Address{Interface: "lan0", CIDR: address}},
		{ID: "lan-client", Owner: "lan", DependsOn: []string{"lan"}, Service: &serviceconfig.Service{Name: "lan-client", DHCP4: &serviceconfig.DHCP4{Interface: "lan0"}}},
		{ID: "unrelated", Interface: &network.Interface{Name: "other0", MTU: 1500}},
		{ID: "lan", Interface: &network.Interface{Name: "lan0", MTU: 1500}},
	}}
}

func assertResourceProjection(t *testing.T, d network.Desired, address, gateway string) {
	t.Helper()
	interfaces := map[string]network.Interface{}
	for _, iface := range d.Interfaces {
		interfaces[iface.Name] = iface
	}
	if len(interfaces) != 2 || !reflect.DeepEqual(interfaces["lan0"].Addresses, []string{address}) || !reflect.DeepEqual(interfaces["other0"].Addresses, []string{"203.0.113.1/24"}) {
		t.Fatalf("resource address projection lost an owner: %+v", d)
	}
	if len(d.Routes) != 1 || d.Routes[0].Gateway != gateway || d.Routes[0].Interface != "lan0" {
		t.Fatalf("dependent route does not match the source program: %+v", d.Routes)
	}
}

func TestResourceProgramUpdateRemovalAndWorkerRecovery(t *testing.T) {
	ctx := context.Background()
	n := &memoryNetwork{}
	m := &memoryManager{}
	runtimeDir, stateDir := t.TempDir(), t.TempDir()
	r := New(runtimeDir, stateDir, n)
	r.manager = m
	wantAddress, wantGateway := "192.0.2.1/24", "192.0.2.2"
	starts := 0
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 0 {
			return
		}
		starts++
		// A declared worker must not start before its complete network intent
		// has passed the single network writer's acknowledgement boundary.
		assertResourceProjection(t, n.applied, wantAddress, wantGateway)
	}
	source := sourceProgram(wantAddress, wantGateway)
	before, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	assertResourceProjection(t, n.applied, wantAddress, wantGateway)
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("compilation or projection mutated operator source intent")
	}
	if starts != 1 || r.State().Revision != 1 || r.State().Phase != "ready" {
		t.Fatal("source program did not reach runtime readiness", r.State(), starts)
	}

	wantAddress, wantGateway = "192.0.2.9/24", "192.0.2.10"
	source = sourceProgram(wantAddress, wantGateway)
	if err := r.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	assertResourceProjection(t, n.applied, wantAddress, wantGateway)

	// A control-worker restart recompiles committed source and quiesces the
	// previous process set before reacquiring protocol producer authority.
	quiesced := false
	previousHook := m.before
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) == 0 {
			quiesced = true
		} else if !quiesced {
			t.Fatal("recovery retained an old producer without quiescing it")
		}
		previousHook(modules)
	}
	recovered := New(runtimeDir, stateDir, n)
	recovered.manager = m
	if err := recovered.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	assertResourceProjection(t, n.applied, wantAddress, wantGateway)
	if !quiesced || recovered.State().Phase != "ready" || len(recovered.records) != 0 {
		t.Fatal("recovery reused unacknowledged protocol observations", recovered.State())
	}

	// Removing this entire owner subtree must leave the other owner's
	// address and interface in the complete desired projection.
	remaining := config.Document{SchemaVersion: config.SchemaVersion}
	for _, resource := range source.Resources {
		if resource.ID == "unrelated" || resource.Owner == "unrelated" {
			remaining.Resources = append(remaining.Resources, resource)
		}
	}
	if err := recovered.ApplyConfig(ctx, 3, remaining); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Interfaces) != 1 || n.applied.Interfaces[0].Name != "other0" || !reflect.DeepEqual(n.applied.Interfaces[0].Addresses, []string{"203.0.113.1/24"}) || len(n.applied.Routes) != 0 || len(m.modules) != 0 {
		t.Fatal("owner removal retained dependent intent or removed unrelated intent", n.applied, m.modules)
	}
}

func TestInvalidResourceGraphHasNoRuntimeSideEffects(t *testing.T) {
	ctx := context.Background()
	for _, initialized := range []bool{false, true} {
		name := "new-runtime"
		if initialized {
			name = "running-runtime"
		}
		t.Run(name, func(t *testing.T) {
			n := &memoryNetwork{}
			m := &memoryManager{}
			dir := t.TempDir()
			r := New(dir, t.TempDir(), n)
			r.manager = m
			if initialized {
				if err := r.ApplyConfig(ctx, 1, sourceProgram("192.0.2.1/24", "192.0.2.2")); err != nil {
					t.Fatal(err)
				}
			}
			calls, state := n.calls, r.State()
			reconciles := 0
			m.before = func([]supervisor.DynamicModule) { reconciles++ }
			invalid := sourceProgram("192.0.2.9/24", "192.0.2.10")
			invalid.Resources[0].DependsOn = []string{"missing-address"}
			if err := r.ApplyConfig(ctx, 2, invalid); err == nil {
				t.Fatal("missing resource dependency was accepted")
			}
			if n.calls != calls || reconciles != 0 || !reflect.DeepEqual(r.State(), state) {
				t.Fatal("invalid graph changed kernel, process, or runtime state")
			}
			if !initialized {
				if _, err := os.Stat(filepath.Join(dir, "instances")); !os.IsNotExist(err) {
					t.Fatal("invalid graph wrote private instance files", err)
				}
			}
		})
	}
}

func TestResourceProgramDoesNotStartWorkerWithoutNetworkAcknowledgement(t *testing.T) {
	ctx := context.Background()
	injected := errors.New("network transaction failed")
	n := &memoryNetwork{fail: injected}
	m := &memoryManager{}
	starts := 0
	m.before = func(modules []supervisor.DynamicModule) {
		if len(modules) != 0 {
			starts++
		}
	}
	r := New(t.TempDir(), t.TempDir(), n)
	r.manager = m
	source := sourceProgram("192.0.2.1/24", "192.0.2.2")
	if err := r.ApplyConfig(ctx, 1, source); !errors.Is(err, injected) {
		t.Fatal("failed network transaction was acknowledged", err)
	}
	if starts != 0 || len(m.modules) != 0 || r.State().Phase != "degraded" {
		t.Fatal("workers started without their network prerequisites", m.modules, r.State())
	}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal("source program could not recover after a network failure", err)
	}
	if starts != 1 || r.State().Phase != "ready" {
		t.Fatal("recovered network transaction did not release worker startup", r.State())
	}
}
