package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"golang.org/x/sys/unix"
)

func deadlineLive(until, now time.Time) bool { return until.IsZero() || now.Before(until) }
func deadlineMinimum(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if !value.IsZero() && (result.IsZero() || value.Before(result)) {
			result = value
		}
	}
	return result
}

// Dependency construction is pure and uses only explicitly authorized facts.
// The caller additionally gates Source against the acknowledged kernel view.
func aftrDependencies(doc config.Native, s serviceconfig.Service, records map[string]serviceconfig.Update, digest string, now time.Time) (aftr.Input, error) {
	c := s.DSLite
	in := aftr.Input{Resource: s.Name, Interface: c.Interface, ConfigDigest: digest, Generation: 1, UpdatedAt: now.UTC(), Active: true}
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
					in.Source = aftr.Source{Address: c.LocalAddress, PreferredUntil: o.PreferredUntil, ValidUntil: o.ValidUntil}
				}
			}
		}
	} else if u, ok := records["service."+c.SourceFrom]; ok && u.Role == "ra_client" {
		var snap raclient.Snapshot
		if err := json.Unmarshal(u.Snapshot, &snap); err != nil {
			return in, err
		}
		if snap.Active {
			var sources []aftr.Source
			for _, address := range snap.Addresses {
				if address.Deprecated || address.DADFailed || !deadlineLive(address.PreferredUntil, now) || !deadlineLive(address.ValidUntil, now) {
					continue
				}
				p, err := netip.ParsePrefix(address.Address)
				if err != nil || !p.Addr().IsGlobalUnicast() {
					continue
				}
				sources = append(sources, aftr.Source{Address: p.Addr().String(), PreferredUntil: address.PreferredUntil, ValidUntil: address.ValidUntil})
			}
			sort.Slice(sources, func(i, j int) bool { return sources[i].Address < sources[j].Address })
			if len(sources) != 0 {
				in.Source = sources[0]
			}
		}
	}
	information := func(name string) (dhcp6pd.Snapshot, time.Time, bool) {
		u, ok := records["service."+name]
		var snap dhcp6pd.Snapshot
		if !ok || u.Role != "dhcp6pd" || json.Unmarshal(u.Snapshot, &snap) != nil {
			return snap, time.Time{}, false
		}
		until, usable := informationDeadline(snap, now)
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
	addResolver := func(address string, port uint16, until time.Time) {
		if port == 0 {
			port = 53
		}
		key := fmt.Sprintf("%s:%d", address, port)
		if seen[key] || len(in.Resolvers) >= aftr.MaxResolvers || !deadlineLive(until, now) {
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
		addResolver(resolver.Address, resolver.Port, time.Time{})
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
		if err := json.Unmarshal(u.Snapshot, &snap); err != nil {
			return in, err
		}
		if snap.Active {
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

// AFTRInput increments a dependency generation only on a content change. A
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
	in, err := aftrDependencies(r.config.plan.Native, service, r.observationsLocked(), b.digest, time.Now())
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
		if previous.Generation == ^uint64(0) {
			return in, errors.New("AFTR dependency generation exhausted")
		}
		in.Generation = previous.Generation + 1
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

func projectDSLite(p *projection, doc config.Native, instances map[string]binding, records map[string]serviceconfig.Update, inputs map[string]aftr.Input, now time.Time) error {
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
		current, err := aftrDependencies(doc, service, records, digest, now)
		if err != nil {
			return err
		}
		// An input without a usable observed source cannot install a tunnel.
		if input.Source.Address == "" {
			continue
		}
		if !sameAFTRInput(input, current) {
			continue
		}
		var snap aftr.Snapshot
		if err := strict(u.Snapshot, &snap); err != nil {
			return err
		}
		if !snap.Active || !snap.Available || snap.Generation != input.Generation || !deadlineLive(snap.ValidUntil, now) {
			continue
		}
		cfg := service.AFTRConfig()
		cfg.ConfigDigest = digest
		if err := aftr.ValidateSnapshot(cfg, input, snap); err != nil {
			return err
		}
		p.network.DSLite = append(p.network.DSLite, network.DSLite{Name: c.Tunnel, Underlay: c.Interface, Local: snap.Local, Remote: snap.Remote, MTU: c.MTU})
		ensureInterface(&p.network, c.Tunnel)
		address := c.B4Address
		if address == "" {
			address = "192.0.0.2/32"
		}
		if err := addAddressWithOptions(&p.network, c.Tunnel, address, network.AddressOptions{Peer: c.B4Peer, PreferredUntil: snap.ValidUntil, ValidUntil: snap.ValidUntil}); err != nil {
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
