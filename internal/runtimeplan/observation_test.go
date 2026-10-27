package runtimeplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"golang.org/x/sys/unix"
)

func TestAFTRSourceIndependentOfUnrelatedLargeNetworkObservations(t *testing.T) {
	n := &aftrNetwork{observed: network.Snapshot{Interfaces: []network.InterfaceState{{Name: "wan0", Index: 42, Up: true, Addresses: []network.AddressState{{Address: "2001:db8:42::1/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}}}}}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	if err := r.ApplyConfig(context.Background(), 1, config.FromNative(aftrDocument())); err != nil {
		t.Fatal(err)
	}
	first, err := r.AFTRInput(context.Background(), "service.dslite")
	if err != nil || first.Source.Address != "2001:db8:42::1" || first.Generation != 1 {
		t.Fatal(first, err)
	}
	for i := range 16000 {
		n.observed.Routes = append(n.observed.Routes, network.RouteState{Route: network.Route{Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255), Interface: "unrelated0", Gateway: "192.0.2.1", Metric: 100, Table: 254}, Protocol: 99, Type: 1})
	}
	n.observed.ObservationErrors = []string{"unrelated WireGuard interface unavailable"}
	full, _ := json.Marshal(n.observed)
	if len(full) <= localapi.MaxBody {
		t.Fatal("fixture does not reproduce oversized full-snapshot failure", len(full))
	}
	input, err := r.AFTRInput(context.Background(), "service.dslite")
	if err != nil || input.Source != first.Source || input.Generation != first.Generation || n.sourceCalls != 2 {
		t.Fatal("unrelated observation size or failure withdrew a valid WAN source", input, err)
	}
	// A failed read supplies no new observation. The worker retains the original
	// dependency deadlines; an observed missing/unusable address still withdraws.
	n.sourceErr = errors.New("WAN address observation unavailable")
	_, err = r.AFTRInput(context.Background(), "service.dslite")
	if !errors.Is(err, n.sourceErr) || !reflect.DeepEqual(r.aftrInputs["service.dslite"], first) {
		t.Fatal("failed source query replaced an accepted dependency", err)
	}
	n.sourceErr = nil
	input, err = r.AFTRInput(context.Background(), "service.dslite")
	if err != nil || input.Source != first.Source || input.Generation != first.Generation {
		t.Fatal("source read recovery changed an unchanged dependency", input, err)
	}
}

func TestAFTRSourceObservationRequiresExactUsableOwnedAddress(t *testing.T) {
	in := aftr.Input{Interface: "wan0", Source: aftr.Source{Address: "2001:db8::1"}}
	baseline := network.SourceObservation{Interface: "wan0", RequestedAddress: "2001:db8::1", InterfacePresent: true, Index: 42, Up: true, Found: true, Address: &network.AddressState{Address: "2001:db8::1/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}
	if !sourceInstalled(in.Interface, in.Source.Address, baseline) {
		t.Fatal("owned usable source was rejected")
	}
	cases := []struct {
		name   string
		change func(*network.SourceObservation)
	}{
		{"interface", func(v *network.SourceObservation) { v.Interface = "wan1" }},
		{"requested-address", func(v *network.SourceObservation) { v.RequestedAddress = "2001:db8::2" }},
		{"missing-interface", func(v *network.SourceObservation) { v.InterfacePresent = false }},
		{"zero-index", func(v *network.SourceObservation) { v.Index = 0 }},
		{"down", func(v *network.SourceObservation) { v.Up = false }},
		{"not-found", func(v *network.SourceObservation) { v.Found = false }},
		{"missing-address", func(v *network.SourceObservation) { v.Address = nil }},
		{"wrong-address", func(v *network.SourceObservation) { v.Address.Address = "2001:db8::2/64" }},
		{"malformed-address", func(v *network.SourceObservation) { v.Address.Address = "2001:db8::1" }},
		{"foreign", func(v *network.SourceObservation) { v.Address.Owned = false }},
		{"deprecated-lifetime", func(v *network.SourceObservation) { v.Address.PreferredLifetime = 0 }},
		{"expired", func(v *network.SourceObservation) { v.Address.ValidLifetime = 0 }},
		{"tentative", func(v *network.SourceObservation) { v.Address.Flags = unix.IFA_F_TENTATIVE }},
		{"dad-failed", func(v *network.SourceObservation) { v.Address.Flags = unix.IFA_F_DADFAILED }},
		{"deprecated-flag", func(v *network.SourceObservation) { v.Address.Flags = unix.IFA_F_DEPRECATED }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := baseline
			address := *baseline.Address
			value.Address = &address
			tc.change(&value)
			if sourceInstalled(in.Interface, in.Source.Address, value) {
				t.Fatal("unusable source accepted")
			}
		})
	}
}

func TestRuntimeHealthDoesNotEncodeOrCopyProtocolSnapshots(t *testing.T) {
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.config.revision, r.phase, r.lastError = 17, "degraded", strings.Repeat("<診断>", 10000)
	payload := json.RawMessage(`{"private_observation":"` + strings.Repeat("x", 64<<10) + `"}`)
	for i := range 1000 {
		name := fmt.Sprintf("service.test-%d", i)
		r.bindings[name] = binding{instance: serviceconfig.Instance{Name: name}}
		r.records[name] = serviceconfig.Update{Snapshot: payload}
	}
	reason := strings.Repeat("diagnostic", 2000)
	for range 10000 {
		r.routeIssues = append(r.routeIssues, RouteIssue{Reason: reason})
	}
	var health HealthStatus
	allocations := testing.AllocsPerRun(10, func() { health = r.Health() })
	if allocations > 1 {
		t.Fatal("health allocation scaled with snapshots/route issue details", allocations)
	}
	if health.Revision != 17 || health.Phase != "degraded" || health.InstanceCount != 1000 || health.RouteIssueCount != 10000 || !health.LastErrorTruncated || len(health.LastError) > 4096 {
		t.Fatal("health lost metadata or its bound", health)
	}
	encoded, err := json.Marshal(health)
	if err != nil || len(encoded) > 32<<10 || bytes.Contains(encoded, []byte("private_observation")) || bytes.Contains(encoded, []byte(`"instances"`)) || bytes.Contains(encoded, []byte(`"route_issues"`)) {
		t.Fatal("health leaked heavy state", len(encoded), err)
	}
}
