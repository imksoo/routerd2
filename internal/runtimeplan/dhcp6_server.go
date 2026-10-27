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
	iface   string
	sources []string
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
	for _, source := range plan.sources {
		u, has := r.records[source]
		bound, configured := r.bindings[source]
		if !has || !configured || u.ConfigDigest != bound.digest {
			continue
		}
		add := func(target *[]dhcp6serverconfig.Address, value, iface string, until lifetime.Deadline) {
			ip, err := netip.ParseAddr(value)
			if err != nil || !dhcp6serverconfig.UsableAddress(ip) || !until.Live(clock) || ip.IsLinkLocalUnicast() && iface != plan.iface {
				return
			}
			for i, entry := range *target {
				if entry.Address == ip {
					if entry.ValidUntil.Nanos != 0 && (until.Nanos == 0 || until.Nanos > entry.ValidUntil.Nanos) {
						(*target)[i].ValidUntil = until
					}
					return
				}
			}
			if len(*target) < 32 {
				*target = append(*target, dhcp6serverconfig.Address{Address: ip, ValidUntil: until})
			}
		}
		addDomain := func(name string, until lifetime.Deadline) {
			if !until.Live(clock) {
				return
			}
			name = strings.TrimSuffix(strings.ToLower(name), ".")
			for i, entry := range out.Options.DomainSearch {
				if entry.Name == name {
					if entry.ValidUntil.Nanos != 0 && (until.Nanos == 0 || until.Nanos > entry.ValidUntil.Nanos) {
						out.Options.DomainSearch[i].ValidUntil = until
					}
					return
				}
			}
			if len(out.Options.DomainSearch) < 32 {
				out.Options.DomainSearch = append(out.Options.DomainSearch, dhcp6serverconfig.Domain{Name: name, ValidUntil: until})
			}
		}
		switch u.Role {
		case "dhcp6":
			var s dhcp6config.Snapshot
			if err := json.Unmarshal(u.Snapshot, &s); err != nil {
				return out, err
			}
			if until, live := s.InformationUntil(clock); live {
				for _, value := range s.DNSServers {
					add(&out.Options.DNSServers, value, s.Interface, until)
				}
				for _, value := range s.SNTPServers {
					add(&out.Options.SNTPServers, value, s.Interface, until)
				}
				for _, value := range s.DomainSearch {
					addDomain(value, until)
				}
			}
		case "ra_client":
			var s raclient.Snapshot
			if err := json.Unmarshal(u.Snapshot, &s); err != nil {
				return out, err
			}
			if s.Active && s.BootID == clock.BootID {
				for _, value := range s.DNSServers {
					add(&out.Options.DNSServers, value.Address, s.Interface, value.ValidUntil)
				}
				for _, value := range s.SearchDomains {
					addDomain(value.Domain, value.ValidUntil)
				}
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
