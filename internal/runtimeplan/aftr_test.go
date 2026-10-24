package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
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
	observed network.Snapshot
}

func (n *aftrNetwork) Snapshot(context.Context) (network.Snapshot, error) { return n.observed, nil }

func aftrDocument() config.Document {
	return config.Document{SchemaVersion: 1, Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8:42::1/64"}}}}, Services: []serviceconfig.Service{{Name: "dslite", DSLite: &serviceconfig.DSLite{Interface: "wan0", Tunnel: "dslite0", LocalAddress: "2001:db8:42::1", Candidates: []string{"2001:db8:42::2"}}}}}
}

func TestAFTRObservedSourceGenerationAndProjection(t *testing.T) {
	ctx := context.Background()
	doc := aftrDocument()
	n := &aftrNetwork{observed: network.Snapshot{Interfaces: []network.InterfaceState{{Name: "wan0", Up: true, Addresses: []network.AddressState{{Address: "2001:db8:42::1/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0), Flags: unix.IFA_F_TENTATIVE}}}}}}
	r := New(t.TempDir(), t.TempDir(), n)
	r.manager = &memoryManager{}
	if err := r.ApplyConfig(ctx, 1, doc); err != nil {
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
	if err := r.ApplyConfig(ctx, 2, doc); err != nil {
		t.Fatal(err)
	}
	if r.bindings["service.dslite"].digest == prior || len(r.aftrInputs) != 0 || len(n.applied.DSLite) != 0 {
		t.Fatal("changed dependency policy reused old instance or tunnel")
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
	cases := map[string]func(*config.Document){
		"uncommitted source": func(d *config.Document) { d.Network.Interfaces[0].Addresses = nil },
		"unscoped resolver": func(d *config.Document) {
			d.Services[0].DSLite.Resolvers = []serviceconfig.AFTRResolver{{Address: "::1"}}
		},
		"host resolver hostname": func(d *config.Document) {
			d.Services[0].DSLite.Resolvers = []serviceconfig.AFTRResolver{{Address: "resolver.example"}}
		},
		"source kind": func(d *config.Document) {
			d.Services[0].DSLite.LocalAddress = ""
			d.Services[0].DSLite.SourceFrom = "dslite"
		},
		"static tunnel collision": func(d *config.Document) {
			d.Network.Interfaces = append(d.Network.Interfaces, network.Interface{Name: "dslite0"})
		},
		"route keeps dynamic tunnel": func(d *config.Document) {
			d.Network.Routes = []network.Route{{Destination: "192.0.2.0/24", Interface: "dslite0"}}
		},
		"underlay cycle": func(d *config.Document) { d.Services[0].DSLite.Interface = "dslite0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := aftrDocument()
			mutate(&d)
			if err := config.Validate(d); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	d := aftrDocument()
	if err := config.Validate(d); err != nil {
		t.Fatal(err)
	}
}
