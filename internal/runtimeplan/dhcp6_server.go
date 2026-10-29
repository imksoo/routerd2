package runtimeplan

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type dhcp6ServerPlan struct {
	selfDNS       []string
	resourceID    string
	iface         string
	dns           []string
	sntp          []string
	search        []string
	dnsDelegation string
}

func (r *Runtime) dhcp6ServerInput(name string) (serviceconfig.DHCP6ServerInput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[name]
	if !ok || binding.instance.Role != "dhcp6_server" {
		return serviceconfig.DHCP6ServerInput{}, errors.New("no configured DHCPv6 server")
	}
	out := serviceconfig.DHCP6ServerInput{ConfigDigest: binding.digest}
	plan := r.config.protocols.dhcp6Servers[name]
	clock, err := boottime.Read()
	if err != nil {
		return out, err
	}
	// This is the last applied source selection, with its original condition
	// deadline. A poll cannot renew it or authorize an unapplied source epoch.
	out.Options.Authority = lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds)}
	state, selected := r.resourceStates[plan.resourceID]
	if r.applied.sourceEpoch != r.sourceEpoch || !selected || !state.Active {
		return out, nil
	}
	out.Options.Authority = state.ActiveUntil
	if out.Options.Authority == (lifetime.Deadline{}) {
		out.Options.Authority.BootID = clock.BootID
	}
	// Decode a shared provider once per input read, while preserving the
	// independently configured ordering of each advertised option.
	sources := make(map[string]dhcp6serverconfig.Input)
	read := func(source string) (dhcp6serverconfig.Input, error) {
		if data, ok := sources[source]; ok {
			return data, nil
		}
		var data dhcp6serverconfig.Input
		u, has := r.records[source]
		bound, configured := r.bindings[source]
		if has && configured && u.ConfigDigest == bound.digest {
			addresses := func(values []string, iface string, until lifetime.Deadline) []dhcp6serverconfig.Address {
				if !until.Live(clock) {
					return nil
				}
				var entries []dhcp6serverconfig.Address
				for _, value := range values {
					ip, err := netip.ParseAddr(value)
					if err == nil && dhcp6serverconfig.UsableAddress(ip) && (!ip.IsLinkLocalUnicast() || iface == plan.iface) {
						entries = append(entries, dhcp6serverconfig.Address{Address: ip, ValidUntil: until})
					}
				}
				return entries
			}
			switch u.Role {
			case "dhcp6":
				var s dhcp6config.Snapshot
				if err := json.Unmarshal(u.Snapshot, &s); err != nil {
					return data, err
				}
				if until, live := s.InformationUntil(clock); live {
					data.DNSServers = addresses(s.DNSServers, s.Interface, until)
					data.SNTPServers = addresses(s.SNTPServers, s.Interface, until)
					for _, name := range s.DomainSearch {
						data.DomainSearch = append(data.DomainSearch, dhcp6serverconfig.Domain{Name: name, ValidUntil: until})
					}
				}
			case "ra_client":
				var s raclient.Snapshot
				if err := json.Unmarshal(u.Snapshot, &s); err != nil {
					return data, err
				}
				if s.Active && s.BootID == clock.BootID {
					for _, value := range s.DNSServers {
						data.DNSServers = append(data.DNSServers, addresses([]string{value.Address}, s.Interface, value.ValidUntil)...)
					}
					for _, value := range s.SearchDomains {
						if value.ValidUntil.Live(clock) {
							data.DomainSearch = append(data.DomainSearch, dhcp6serverconfig.Domain{Name: value.Domain, ValidUntil: value.ValidUntil})
						}
					}
				}
			}
		}
		sources[source] = data
		return data, nil
	}
	addAddresses := func(target *[]dhcp6serverconfig.Address, entries []dhcp6serverconfig.Address) {
		for _, entry := range entries {
			found := false
			for i, old := range *target {
				if old.Address == entry.Address {
					if old.ValidUntil.Nanos != 0 && (entry.ValidUntil.Nanos == 0 || entry.ValidUntil.Nanos > old.ValidUntil.Nanos) {
						(*target)[i].ValidUntil = entry.ValidUntil
					}
					found = true
					break
				}
			}
			if !found && len(*target) < 32 {
				*target = append(*target, entry)
			}
		}
	}
	listening, err := listeningDNS(plan.selfDNS, r.records, clock)
	if err != nil {
		return out, err
	}
	delegatedReady := false
	var readyUntil lifetime.Deadline
	for _, entry := range r.delegatedDNS[plan.dnsDelegation] {
		address := entry.address
		if !address.ValidUntil.Live(clock) {
			continue
		}
		if len(plan.selfDNS) > 0 {
			until, found := dnsReadyUntil(address.Address, listening, clock)
			if !found {
				continue
			}
			address.ValidUntil = lifetime.Minimum(clock, address.ValidUntil, until)
		}
		addAddresses(&out.Options.DNSServers, []dhcp6serverconfig.Address{address})
		if !delegatedReady || address.ValidUntil.Remaining(clock) > readyUntil.Remaining(clock) {
			readyUntil = address.ValidUntil
		}
		delegatedReady = true
	}
	// A prefix installed in the kernel is not yet a working self-DNS service.
	// Await DAD and actual sockets, then retain both original deadlines through
	// the worker's receive wait. A stale report cannot authorize an empty Reply.
	if plan.dnsDelegation != "" {
		if !delegatedReady {
			out.Options.Authority = lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds)}
		} else {
			out.Options.Authority = lifetime.Minimum(clock, out.Options.Authority, readyUntil)
		}
	}
	for _, name := range plan.dns {
		data, err := read(name)
		if err != nil {
			return out, err
		}
		addAddresses(&out.Options.DNSServers, data.DNSServers)
	}
	for _, name := range plan.sntp {
		data, err := read(name)
		if err != nil {
			return out, err
		}
		addAddresses(&out.Options.SNTPServers, data.SNTPServers)
	}
	for _, name := range plan.search {
		data, err := read(name)
		if err != nil {
			return out, err
		}
		for _, entry := range data.DomainSearch {
			entry.Name = strings.TrimSuffix(strings.ToLower(entry.Name), ".")
			found := false
			for i, old := range out.Options.DomainSearch {
				if old.Name == entry.Name {
					if old.ValidUntil.Nanos != 0 && (entry.ValidUntil.Nanos == 0 || entry.ValidUntil.Nanos > old.ValidUntil.Nanos) {
						out.Options.DomainSearch[i].ValidUntil = entry.ValidUntil
					}
					found = true
					break
				}
			}
			if !found && len(out.Options.DomainSearch) < 32 {
				out.Options.DomainSearch = append(out.Options.DomainSearch, entry)
			}
		}
	}
	return out, nil
}

func (r *Runtime) dhcp6ServerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/dhcp6-server-input", func(w http.ResponseWriter, req *http.Request) {
		out, err := r.dhcp6ServerInput(req.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 404, err)
			return
		}
		localapi.JSON(w, 200, out)
	})
}
