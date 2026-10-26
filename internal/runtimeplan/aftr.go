package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"golang.org/x/sys/unix"
)

func deadlineLive(until, now time.Time) bool { return until.IsZero() || now.Before(until) }

// Dependency construction is pure and uses only explicitly authorized facts.
// The caller additionally gates Source against the acknowledged kernel view.
func aftrDependencies(doc config.Native, s serviceconfig.Service, records map[string]serviceconfig.Update, digest string, clock boottime.Clock) (aftr.Input, error) {
	now := clock.WallTime
	c := s.DSLite
	in := aftr.Input{Resource: s.Name, Interface: c.Interface, ConfigDigest: digest, Generation: 1, UpdatedAt: now.UTC(), Active: true}
	var sourceRA raclient.Snapshot
	if c.LocalAddress != "" {
		for _, iface := range doc.Network.Interfaces {
			if iface.Name != c.Interface {
				continue
			}
			for _, address := range iface.Addresses {
				p, err := netip.ParsePrefix(address)
				if err != nil || p.Addr().String() != c.LocalAddress {
					continue
				}
				o := iface.AddressOptions[address]
				if deadlineLive(o.PreferredUntil, now) && deadlineLive(o.ValidUntil, now) {
					in.Source = aftr.Source{Address: c.LocalAddress, PreferredUntil: lifetime.Deadline{UTC: o.PreferredUntil}, ValidUntil: lifetime.Deadline{UTC: o.ValidUntil}}
				}
			}
		}
	} else if u, ok := records["service."+c.SourceFrom]; ok && u.Role == "ra_client" {
		if err := json.Unmarshal(u.Snapshot, &sourceRA); err != nil {
			return in, err
		}
		if sourceRA.Active && sourceRA.BootID == clock.BootID && sourceRA.ObservedNanos <= uint64(clock.BoottimeNanoseconds) {
			for _, address := range sourceRA.Addresses {
				if address.Deprecated || address.DADFailed || !address.PreferredUntil.Live(clock) || !address.ValidUntil.Live(clock) {
					continue
				}
				p, err := netip.ParsePrefix(address.Address)
				if err != nil || !p.Addr().IsGlobalUnicast() {
					continue
				}
				candidate := p.Addr().String()
				if in.Source.Address == "" || candidate < in.Source.Address {
					in.Source = aftr.Source{Address: candidate, PreferredUntil: address.PreferredUntil, ValidUntil: address.ValidUntil}
				}
			}
		}
	}
	information := func(name string) (dhcp6config.Snapshot, lifetime.Deadline, bool) {
		u, ok := records["service."+name]
		var snap dhcp6config.Snapshot
		if !ok || u.Role != "dhcp6" || json.Unmarshal(u.Snapshot, &snap) != nil {
			return snap, lifetime.Deadline{}, false
		}
		until, usable := snap.InformationUntil(clock)
		return snap, until, usable
	}
	if c.AFTRFrom != "" {
		if snap, until, usable := information(c.AFTRFrom); usable && snap.AFTRName != "" {
			in.Candidates = append(in.Candidates, aftr.Candidate{Value: snap.AFTRName, Origin: "dhcp6", ValidUntil: until})
		}
	}
	for _, candidate := range c.Candidates {
		in.Candidates = append(in.Candidates, aftr.Candidate{Value: candidate, Origin: "configured"})
	}
	seen := map[string]bool{}
	addResolver := func(address string, port uint16, until lifetime.Deadline) {
		if port == 0 {
			port = 53
		}
		key := fmt.Sprintf("%s:%d", address, port)
		if seen[key] || len(in.Resolvers) >= aftr.MaxResolvers || !until.Live(clock) {
			return
		}
		a, err := netip.ParseAddr(address)
		if err != nil || !a.Is6() || a.Is4In6() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.Zone() != "" {
			return
		}
		seen[key] = true
		in.Resolvers = append(in.Resolvers, aftr.Resolver{Address: a.String(), Port: port, Interface: c.Interface, ValidUntil: until})
	}
	for _, resolver := range c.Resolvers {
		addResolver(resolver.Address, resolver.Port, lifetime.Deadline{})
	}
	for _, name := range c.DNSFrom {
		if snap, until, usable := information(name); usable {
			for _, server := range snap.DNSServers {
				addResolver(server, 53, until)
			}
			continue
		}
		u, ok := records["service."+name]
		if !ok || u.Role != "ra_client" {
			continue
		}
		var snap raclient.Snapshot
		if name == c.SourceFrom {
			snap = sourceRA
		} else {
			if err := json.Unmarshal(u.Snapshot, &snap); err != nil {
				return in, err
			}
		}
		if snap.Active && snap.BootID == clock.BootID && snap.ObservedNanos <= uint64(clock.BoottimeNanoseconds) {
			for _, server := range snap.DNSServers {
				addResolver(server.Address, 53, server.ValidUntil)
			}
		}
	}
	cfg := s.AFTRConfig()
	cfg.ConfigDigest = digest
	return in, aftr.ValidateInput(cfg, in)
}

func sameAFTRInput(a, b aftr.Input) bool {
	a.Generation, b.Generation = 0, 0
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func sourceInstalled(in aftr.Input, observed network.SourceObservation) bool {
	if in.Source.Address == "" || observed.Interface != in.Interface || observed.RequestedAddress != in.Source.Address || !observed.InterfacePresent || observed.Index <= 0 || !observed.Up || !observed.Found || observed.Address == nil {
		return false
	}
	address := observed.Address
	p, err := netip.ParsePrefix(address.Address)
	return err == nil && p.Addr().String() == in.Source.Address && address.Owned && address.PreferredLifetime > 0 && address.ValidLifetime > 0 && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0
}

// AFTRInput increments generation when endpoint dependencies change. A
// poll cannot renew DHCP, RA or DNS lifetimes. DAD completion is observed by
// the network worker, which is the only process allowed to mutate networking.
func (r *Runtime) AFTRInput(ctx context.Context, name string) (aftr.Input, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bindings[name]
	if !r.configured || !ok || b.instance.Role != "aftr" {
		return aftr.Input{}, errors.New("no committed AFTR consumer")
	}
	var service serviceconfig.Service
	for _, s := range r.config.plan.Native.Services {
		if s.ModuleName() == name {
			service = s
			break
		}
	}
	if service.DSLite == nil {
		return aftr.Input{}, errors.New("missing DS-Lite intent")
	}
	clock, err := boottime.Read()
	if err != nil {
		return aftr.Input{}, err
	}
	in, err := aftrDependencies(r.config.plan.Native, service, r.observationsLocked(), b.digest, clock)
	if err != nil {
		return in, err
	}
	observer, ok := r.network.(interface {
		SourceAddress(context.Context, string, string) (network.SourceObservation, error)
	})
	if !ok {
		return in, errors.New("AFTR requires native source address observation")
	}
	if in.Source.Address != "" {
		// Unrelated routing tables and WireGuard peers cannot invalidate this
		// dependency merely by exceeding the operator observation response cap.
		observed, err := observer.SourceAddress(ctx, in.Interface, in.Source.Address)
		if err != nil || !sourceInstalled(in, observed) {
			in.Source = aftr.Source{}
		}
	}
	if previous, ok := r.aftrInputs[name]; ok && previous.ConfigDigest == in.ConfigDigest {
		if sameAFTRInput(previous, in) {
			return previous, nil
		}
		in.Generation = previous.Generation
		if !aftr.SameQuery(previous, in) {
			if previous.Generation == ^uint64(0) {
				return in, errors.New("AFTR dependency generation exhausted")
			}
			in.Generation++
		}
	}
	if r.aftrInputs == nil {
		r.aftrInputs = map[string]aftr.Input{}
	}
	r.aftrInputs[name] = in
	return in, nil
}

func (r *Runtime) aftrRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/aftr-input", func(w http.ResponseWriter, req *http.Request) {
		in, err := r.AFTRInput(req.Context(), req.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 409, err)
			return
		}
		localapi.JSON(w, 200, in)
	})
}

func projectDSLite(p *projection, doc config.Native, instances map[string]binding, records map[string]serviceconfig.Update, inputs map[string]aftr.Input, clock boottime.Clock) error {
	for _, service := range doc.Services {
		if service.DSLite == nil {
			continue
		}
		c := service.DSLite
		input, exists := inputs[service.ModuleName()]
		u, has := records[service.ModuleName()]
		if !exists || !has {
			continue
		}
		digest := instances[service.ModuleName()].digest
		if input.ConfigDigest != digest {
			continue
		}
		current, err := aftrDependencies(doc, service, records, digest, clock)
		if err != nil {
			return err
		}
		// An input without a usable observed source cannot install a tunnel.
		if input.Source.Address == "" {
			continue
		}
		if !aftr.SameQuery(input, current) {
			continue
		}
		var snap aftr.Snapshot
		if err := strict(u.Snapshot, &snap); err != nil {
			return err
		}
		if !snap.Active || !snap.Available || snap.Generation != input.Generation || snap.BootID != clock.BootID || !snap.ValidUntil.Live(clock) {
			continue
		}
		cfg := service.AFTRConfig()
		cfg.ConfigDigest = digest
		if err := aftr.ValidateSnapshot(cfg, input, snap); err != nil {
			return err
		}
		// Use current source budgets immediately, including shortening, without
		// waiting for the resolver to republish an unchanged DNS endpoint.
		until := lifetime.Minimum(clock, snap.ValidUntil, current.Source.PreferredUntil, current.Source.ValidUntil, current.Candidates[snap.CandidateIndex].ValidUntil)
		if snap.ResolverIndex >= 0 {
			until = lifetime.Minimum(clock, until, current.Resolvers[snap.ResolverIndex].ValidUntil)
		}
		if !until.Live(clock) {
			continue
		}
		p.network.Links = append(p.network.Links, network.Link{Name: c.Tunnel, Kind: "ip6tnl", Parent: c.Interface, Local: snap.Local, Remote: snap.Remote, Authority: until})
		p.network.Interfaces = append(p.network.Interfaces, network.Interface{Name: c.Tunnel, MTU: c.MTU})
		address := c.B4Address
		if address == "" {
			address = "192.0.0.2/32"
		}
		options := addressLifetimes(until, until)
		options.Peer = c.B4Peer
		if err := addAddressWithOptions(&p.network, c.Tunnel, address, options); err != nil {
			return err
		}
		if c.DefaultRoute == nil || *c.DefaultRoute {
			p.network.Routes = append(p.network.Routes, network.Route{Destination: "0.0.0.0/0", Interface: c.Tunnel, Gateway: c.RouteGateway, Metric: c.Metric, Table: c.Table})
		}
	}
	return nil
}

func (r *Runtime) validateAFTRSnapshotLocked(b binding, raw json.RawMessage) error {
	var private serviceconfig.DSLiteInstance
	var snap aftr.Snapshot
	if err := strict(b.instance.Config, &private); err != nil {
		return err
	}
	if err := strict(raw, &snap); err != nil {
		return err
	}
	in, ok := r.aftrInputs[b.instance.Name]
	if !ok || in.ConfigDigest != b.digest || snap.Generation != in.Generation {
		return ErrStale
	}
	private.Protocol.ConfigDigest = b.digest
	return aftr.ValidateSnapshot(private.Protocol, in, snap)
}
