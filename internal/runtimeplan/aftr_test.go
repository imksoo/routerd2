package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"

	old6 "github.com/imksoo/routerd/pkg/pdclient"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"golang.org/x/sys/unix"
)

type aftrNetwork struct {
	memoryNetwork
	observed    network.Snapshot
	sourceErr   error
	sourceCalls int
}

func (n *aftrNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	panic("AFTR must not query the full operator snapshot")
}

func (n *aftrNetwork) SourceAddress(_ context.Context, name, requested string) (network.SourceObservation, error) {
	n.sourceCalls++
	value := network.SourceObservation{Interface: name, RequestedAddress: requested}
	if n.sourceErr != nil {
		return value, n.sourceErr
	}
	for _, iface := range n.observed.Interfaces {
		if iface.Name != name {
			continue
		}
		value.InterfacePresent, value.Index, value.Up = true, iface.Index, iface.Up
		for _, address := range iface.Addresses {
			prefix, err := netip.ParsePrefix(address.Address)
			if err == nil && prefix.Addr().String() == requested {
				copy := address
				value.Found, value.Address = true, &copy
				return value, nil
			}
		}
	}
	return value, nil
}

func aftrDocument() config.Native {
	return config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8:42::1/64"}}}}, Services: []serviceconfig.Service{{Name: "dslite", DSLite: &serviceconfig.DSLite{Interface: "wan0", Tunnel: "dslite0", LocalAddress: "2001:db8:42::1", Candidates: []string{"2001:db8:42::2"}}}}}
}

func TestAFTRObservedSourceGenerationAndProjection(t *testing.T) {
	ctx := context.Background()
	doc := aftrDocument()
	n := &aftrNetwork{observed: network.Snapshot{Interfaces: []network.InterfaceState{{Name: "wan0", Index: 42, Up: true, Addresses: []network.AddressState{{Address: "2001:db8:42::1/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0), Flags: unix.IFA_F_TENTATIVE}}}}}}
	r := New(t.TempDir(), t.TempDir(), n)
	r.manager = &memoryManager{}
	if err := r.ApplyConfig(ctx, 1, config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	input, err := r.AFTRInput(ctx, "service.dslite")
	if err != nil || input.Source.Address != "" || input.Generation != 1 {
		t.Fatalf("tentative source escaped gate: %+v %v", input, err)
	}
	n.observed.Interfaces[0].Addresses[0].Flags = 0
	input, err = r.AFTRInput(ctx, "service.dslite")
	if err != nil || input.Source.Address != "2001:db8:42::1" || input.Generation != 2 {
		t.Fatalf("DAD completion missing: %+v %v", input, err)
	}
	again, err := r.AFTRInput(ctx, "service.dslite")
	if err != nil || !reflect.DeepEqual(again, input) {
		t.Fatal("poll renewed an unchanged dependency", again, err)
	}
	snapshot := aftr.Snapshot{Resource: "dslite", Interface: "wan0", ConfigDigest: input.ConfigDigest, Generation: input.Generation, Active: true, Available: true, Local: input.Source.Address, Remote: "2001:db8:42::2", CandidateIndex: 0, ResolverIndex: -1, UpdatedAt: time.Now().UTC()}
	if err := r.Accept(ctx, update(t, r, "service.dslite", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.DSLite) != 1 || len(n.applied.Routes) != 1 || n.applied.DSLite[0].Name != "dslite0" || n.applied.Routes[0].Interface != "dslite0" {
		t.Fatalf("complete B4 projection missing: %+v", n.applied)
	}
	n.observed.Interfaces[0].Addresses[0].Flags = unix.IFA_F_DADFAILED
	input, err = r.AFTRInput(ctx, "service.dslite")
	if err != nil || input.Generation != 3 || input.Source.Address != "" {
		t.Fatal(input, err)
	}
	if err := r.Accept(ctx, update(t, r, "service.dslite", 2, snapshot)); !errors.Is(err, ErrStale) {
		t.Fatal("old dependency result was not fenced", err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.DSLite) != 0 || len(n.applied.Routes) != 0 || len(n.applied.Interfaces) != 1 {
		t.Fatalf("unusable source retained B4 resources: %+v", n.applied)
	}
	n.observed.Interfaces[0].Addresses[0].Flags = 0
	input, err = r.AFTRInput(ctx, "service.dslite")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Generation = input.Generation
	if err := r.Accept(ctx, update(t, r, "service.dslite", 3, snapshot)); err != nil {
		t.Fatal(err)
	}
	prior := r.bindings["service.dslite"].digest
	doc.Services[0].DSLite.Candidates = []string{"2001:db8:42::3"}
	if err := r.ApplyConfig(ctx, 2, config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	if r.bindings["service.dslite"].digest == prior || len(r.aftrInputs) != 0 || len(n.applied.DSLite) != 0 {
		t.Fatal("changed dependency policy reused old instance or tunnel")
	}
}

func TestAFTRPeerAndGatewayChangesFenceOldProjection(t *testing.T) {
	ctx := context.Background()
	n := &aftrNetwork{observed: network.Snapshot{Interfaces: []network.InterfaceState{{Name: "wan0", Index: 42, Up: true, Addresses: []network.AddressState{{Address: "2001:db8:42::1/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}}}}}
	r := New(t.TempDir(), t.TempDir(), n)
	r.manager = &memoryManager{}
	no := false
	var priorDigest string
	var priorUpdate serviceconfig.Update
	steps := []struct {
		name, peer, gateway string
		defaultRoute        *bool
	}{
		{name: "original-default"},
		{name: "peer-only", peer: "192.0.0.1/32"},
		{name: "peer-gateway", peer: "192.0.0.1/32", gateway: "192.0.0.1"},
		{name: "peer-change", peer: "192.0.0.9/32", gateway: "192.0.0.9"},
		{name: "remove-default-route", peer: "192.0.0.9/32", defaultRoute: &no},
		{name: "remove-peer", defaultRoute: &no},
	}
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			doc := aftrDocument()
			doc.Services[0].DSLite.B4Peer = step.peer
			doc.Services[0].DSLite.RouteGateway = step.gateway
			doc.Services[0].DSLite.DefaultRoute = step.defaultRoute
			if err := r.ApplyConfig(ctx, uint64(i+1), config.FromNative(doc)); err != nil {
				t.Fatal(err)
			}
			if len(n.applied.DSLite) != 0 || len(n.applied.Routes) != 0 || len(n.applied.Interfaces) != 1 {
				t.Fatal("changed B4 intent retained old address/route ownership before new facts arrived", n.applied)
			}
			input, err := r.AFTRInput(ctx, "service.dslite")
			if err != nil || input.ConfigDigest == priorDigest || input.Generation != 1 {
				t.Fatal("B4 intent change did not create a new fenced instance", input, err)
			}
			if priorDigest != "" && !errors.Is(r.Accept(ctx, priorUpdate), ErrStale) {
				t.Fatal("an old AFTR producer could recreate the prior peer or route")
			}
			snapshot := aftr.Snapshot{Resource: "dslite", Interface: "wan0", ConfigDigest: input.ConfigDigest, Generation: input.Generation, Active: true, Available: true, Local: input.Source.Address, Remote: "2001:db8:42::2", CandidateIndex: 0, ResolverIndex: -1, UpdatedAt: time.Now().UTC()}
			current := update(t, r, "service.dslite", 1, snapshot)
			if err := r.Accept(ctx, current); err != nil {
				t.Fatal(err)
			}
			if err := network.Validate(n.applied); err != nil {
				t.Fatal("public service produced invalid point-to-point kernel intent", err)
			}
			if len(n.applied.DSLite) != 1 || len(n.applied.Interfaces) != 2 {
				t.Fatal("new AFTR producer did not acquire its complete B4 projection", n.applied)
			}
			var b4 network.Interface
			for _, iface := range n.applied.Interfaces {
				if iface.Name == "dslite0" {
					b4 = iface
				}
			}
			if !reflect.DeepEqual(b4.Addresses, []string{"192.0.0.2/32"}) || b4.AddressOptions["192.0.0.2/32"].Peer != step.peer {
				t.Fatal("explicit peer or original default address was lost", b4)
			}
			if step.defaultRoute != nil && !*step.defaultRoute {
				if len(n.applied.Routes) != 0 {
					t.Fatal("peer alone invented a forbidden default route", n.applied.Routes)
				}
			} else if len(n.applied.Routes) != 1 || n.applied.Routes[0].Destination != "0.0.0.0/0" || n.applied.Routes[0].Interface != "dslite0" || n.applied.Routes[0].Gateway != step.gateway {
				t.Fatal("explicit gateway or original device-route behavior was lost", n.applied.Routes)
			}
			priorDigest, priorUpdate = input.ConfigDigest, current
		})
	}
	if err := r.ApplyConfig(ctx, uint64(len(steps)+1), config.FromNative(config.Native{})); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.DSLite)+len(n.applied.Interfaces)+len(n.applied.Routes) != 0 || len(r.bindings) != 0 || len(r.aftrInputs) != 0 {
		t.Fatal("configuration removal retained B4 resources or an authorized producer", n.applied)
	}
	if !errors.Is(r.Accept(ctx, priorUpdate), ErrStale) {
		t.Fatal("removed AFTR producer reacquired ownership")
	}
}

func TestAFTRFinitePeerAddressAndGatewayWithdrawTogether(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	expiry := now.Add(30 * time.Second)
	doc := aftrDocument()
	doc.Services[0].DSLite.B4Peer = "192.0.0.1/32"
	doc.Services[0].DSLite.RouteGateway = "192.0.0.1"
	doc.Network.Interfaces[0].AddressOptions = map[string]network.AddressOptions{"2001:db8:42::1/64": {PreferredUntil: expiry, ValidUntil: expiry}}
	instance, err := doc.Services[0].Instance("/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := encodeInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	in, err := aftrDependencies(doc, doc.Services[0], nil, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := aftr.Snapshot{Resource: "dslite", Interface: "wan0", ConfigDigest: digest, Generation: in.Generation, Active: true, Available: true, Local: in.Source.Address, Remote: "2001:db8:42::2", CandidateIndex: 0, ResolverIndex: -1, UpdatedAt: now, ValidUntil: expiry}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]serviceconfig.Update{instance.Name: {Role: "aftr", Snapshot: raw}}
	inputs := map[string]aftr.Input{instance.Name: in}
	p, err := project(doc, "/run/routerd", "/var/lib/routerd", records, now, inputs)
	if err != nil || len(p.network.DSLite) != 1 || len(p.network.Routes) != 1 || p.network.Routes[0].Gateway != "192.0.0.1" {
		t.Fatal("finite source did not project its peer route", p.network, err)
	}
	if err := network.Validate(p.network); err != nil {
		t.Fatal(err)
	}
	var options network.AddressOptions
	for _, iface := range p.network.Interfaces {
		if iface.Name == "dslite0" {
			options = iface.AddressOptions["192.0.0.2/32"]
		}
	}
	if options.Peer != "192.0.0.1/32" || !options.PreferredUntil.Equal(expiry) || !options.ValidUntil.Equal(expiry) {
		t.Fatal("peer addressing changed the finite address lifetime", options)
	}
	p, err = project(doc, "/run/routerd", "/var/lib/routerd", records, expiry, inputs)
	if err != nil || len(p.network.DSLite) != 0 || len(p.network.Routes) != 0 || len(p.network.Interfaces) != 1 || p.network.Interfaces[0].Name != "wan0" {
		t.Fatal("source expiry retained peer address, tunnel or gateway route", p.network, err)
	}
}

func TestAFTRFiniteDependencyInvalidatesPriorResolution(t *testing.T) {
	now := time.Now().UTC()
	doc := aftrDocument()
	doc.Services[0].DSLite.AFTRFrom = "wan-info"
	doc.Services[0].DSLite.DNSFrom = []string{"wan-info"}
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "wan-info", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", InformationOnly: true}})
	info := dhcp6pd.Snapshot{Snapshot: old6.Snapshot{Resource: "wan-info", Interface: "wan0", AFTRName: "aftr.example", DNSServers: []string{"2001:db8:42::53"}, InfoUpdatedAt: now}, Active: true, InformationOnly: true, InformationRefreshAt: now.Add(30 * time.Second)}
	raw, _ := json.Marshal(info)
	records := map[string]serviceconfig.Update{"service.wan-info": {Role: "dhcp6pd", Snapshot: raw}}
	instance, err := doc.Services[0].Instance("/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, _ := encodeInstance(instance)
	in, err := aftrDependencies(doc, doc.Services[0], records, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Candidates) != 2 || in.Candidates[0].Value != "aftr.example" || !in.Candidates[0].ValidUntil.Equal(info.InformationRefreshAt) || len(in.Resolvers) != 1 || !in.Resolvers[0].ValidUntil.Equal(info.InformationRefreshAt) {
		t.Fatalf("information lifetime lost: %+v", in)
	}
	snap := aftr.Snapshot{Resource: "dslite", Interface: "wan0", ConfigDigest: digest, Generation: 1, Active: true, Available: true, Local: in.Source.Address, Remote: "2001:db8:42::2", CandidateIndex: 1, ResolverIndex: -1, UpdatedAt: now}
	raw, _ = json.Marshal(snap)
	records[instance.Name] = serviceconfig.Update{Role: "aftr", Snapshot: raw}
	inputs := map[string]aftr.Input{instance.Name: in}
	p, err := project(doc, "/run/routerd", "/var/lib/routerd", records, now, inputs)
	if err != nil || len(p.network.DSLite) != 1 {
		t.Fatal("valid generation failed", p.network, err)
	}
	// Information expires while the lower-priority literal is still static.
	// Reusing candidate index 1 against a changed list would authorize a stale
	// result; only a new dependency generation may select the remaining entry.
	p, err = project(doc, "/run/routerd", "/var/lib/routerd", records, now.Add(31*time.Second), inputs)
	if err != nil || len(p.network.DSLite) != 0 {
		t.Fatal("changed candidate list retained an old generation", p.network, err)
	}
	in, err = aftrDependencies(doc, doc.Services[0], records, digest, now.Add(31*time.Second))
	if err != nil || len(in.Candidates) != 1 || len(in.Resolvers) != 0 {
		t.Fatal("expired DHCP information used", in, err)
	}
}

func TestAFTRExplicitInfiniteInformationRefresh(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	doc := aftrDocument()
	doc.Services[0].DSLite.AFTRFrom = "wan-info"
	doc.Services[0].DSLite.DNSFrom = []string{"wan-info"}
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "wan-info", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", InformationOnly: true}})
	instance, err := doc.Services[0].Instance("/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, _ := encodeInstance(instance)
	info := dhcp6pd.Snapshot{Snapshot: old6.Snapshot{Resource: "wan-info", Interface: "wan0", AFTRName: "aftr.example", DNSServers: []string{"2001:db8:42::53"}, InfoUpdatedAt: now}, Active: true, InformationOnly: true, InformationRefreshSeconds: ^uint32(0)}
	for _, pd := range []bool{false, true} {
		info.InformationOnly = !pd
		want := time.Time{}
		if pd {
			info.ExpiresAt = now.Add(90 * time.Second)
			want = info.ExpiresAt
		}
		raw, _ := json.Marshal(info)
		records := map[string]serviceconfig.Update{"service.wan-info": {Role: "dhcp6pd", Snapshot: raw}}
		in, err := aftrDependencies(doc, doc.Services[0], records, digest, now)
		if err != nil || len(in.Candidates) != 2 || in.Candidates[0].Value != "aftr.example" || len(in.Resolvers) != 1 {
			t.Fatal("explicit infinite refresh lost valid information", in, err)
		}
		if !in.Candidates[0].ValidUntil.Equal(want) || !in.Resolvers[0].ValidUntil.Equal(want) {
			t.Fatal("infinite refresh lost PD lease bound", in)
		}
		if pd {
			in, err = aftrDependencies(doc, doc.Services[0], records, digest, info.ExpiresAt)
			if err != nil || len(in.Candidates) != 1 || len(in.Resolvers) != 0 {
				t.Fatal("infinite refresh outlived its PD lease", in, err)
			}
		}
	}
	info.InformationOnly, info.InformationRefreshSeconds = true, 0
	raw, _ := json.Marshal(info)
	in, err := aftrDependencies(doc, doc.Services[0], map[string]serviceconfig.Update{"service.wan-info": {Role: "dhcp6pd", Snapshot: raw}}, digest, now)
	if err != nil || len(in.Candidates) != 1 || len(in.Resolvers) != 0 {
		t.Fatal("missing refresh timestamp was mistaken for explicit infinity", in, err)
	}
}

func TestDSLiteRejectsUnownedAndCyclicDependencies(t *testing.T) {
	cases := map[string]func(*config.Native){
		"uncommitted source": func(d *config.Native) { d.Network.Interfaces[0].Addresses = nil },
		"unscoped resolver": func(d *config.Native) {
			d.Services[0].DSLite.Resolvers = []serviceconfig.AFTRResolver{{Address: "::1"}}
		},
		"host resolver hostname": func(d *config.Native) {
			d.Services[0].DSLite.Resolvers = []serviceconfig.AFTRResolver{{Address: "resolver.example"}}
		},
		"source kind": func(d *config.Native) {
			d.Services[0].DSLite.LocalAddress = ""
			d.Services[0].DSLite.SourceFrom = "dslite"
		},
		"static tunnel collision": func(d *config.Native) {
			d.Network.Interfaces = append(d.Network.Interfaces, network.Interface{Name: "dslite0"})
		},
		"route keeps dynamic tunnel": func(d *config.Native) {
			d.Network.Routes = []network.Route{{Destination: "192.0.2.0/24", Interface: "dslite0"}}
		},
		"underlay cycle": func(d *config.Native) { d.Services[0].DSLite.Interface = "dslite0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := aftrDocument()
			mutate(&d)
			if err := config.Validate(config.FromNative(d)); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	d := aftrDocument()
	if err := config.Validate(config.FromNative(d)); err != nil {
		t.Fatal(err)
	}
}
