package runtimeplan

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func (r *Runtime) ntpSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/ntp-sources", func(w http.ResponseWriter, request *http.Request) {
		query, queryErr := url.ParseQuery(request.URL.RawQuery)
		generation, generationErr := strconv.ParseUint(query.Get("generation"), 10, 64)
		if queryErr != nil || generationErr != nil || generation == 0 || len(query) != 1 || len(query["generation"]) != 1 {
			localapi.Fail(w, 400, errors.New("NTP source request requires one positive worker generation"))
			return
		}
		r.mu.Lock()
		name := request.PathValue("name")
		node, configured := r.config.protocols.ntp[name]
		binding, selected := r.bindings[name]
		if !configured || !selected {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusNotFound, errors.New("NTP service is not selected"))
			return
		}
		producer := r.guard.modules[name]
		if producer.Generation != generation || producer.PID == 0 || producer.ConfigDigest != binding.digest {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("NTP process authority is unknown"))
			return
		}
		clock, err := boottime.Read()
		if err != nil {
			r.mu.Unlock()
			localapi.Fail(w, 503, err)
			return
		}
		if r.guard.bootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= r.guard.untilNanos {
			r.mu.Unlock()
			localapi.Fail(w, 503, errors.New("NTP source process observation is stale"))
			return
		}
		var facts resourceprogram.FactSnapshot
		for _, group := range node.Sources {
			if group.When == nil {
				continue
			}
			facts, err = r.conditionFactsLocked(r.config)
			if err != nil {
				r.mu.Unlock()
				localapi.Fail(w, 503, err)
				return
			}
			break
		}
		input := ntpconfig.Sources{ConfigDigest: binding.digest, Generation: producer.Generation, Groups: make([]ntpconfig.SourceGrant, len(node.Sources))}
		for i, group := range node.Sources {
			grant := &input.Groups[i]
			if group.When != nil {
				truth, until := facts.ConditionAuthority(*group.When)
				if truth != resourceprogram.True {
					continue
				}
				grant.ActiveUntil = &lifetime.Deadline{BootID: clock.BootID, Nanos: until}
			}
			for _, ref := range group.SourcesFrom {
				moduleName := "service." + ref
				owner, active := r.bindings[moduleName]
				module := r.guard.modules[moduleName]
				record := r.records[moduleName]
				if !active || !module.Ready || module.Generation != record.Generation || module.ConfigDigest != owner.digest || record.ConfigDigest != owner.digest {
					continue
				}
				var servers []string
				var iface string
				var until lifetime.Deadline
				switch record.Role {
				case "dhcp6":
					snapshot := r.guardGrants[moduleName].dhcp6
					if snapshot == nil || snapshot.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
						continue
					}
					var live bool
					until, live = snapshot.InformationUntil(clock)
					if !live {
						continue
					}
					servers, iface = snapshot.SNTPServers, snapshot.Interface
				case "dhcp4":
					snapshot := r.guardGrants[moduleName].dhcp4
					if snapshot == nil {
						continue
					}
					until = lifetime.Deadline{BootID: snapshot.LeaseClock.BootID, Nanos: snapshot.LeaseClock.ExpiresNanos}
					if !snapshot.Active || !until.Live(clock) {
						continue
					}
					servers, iface = snapshot.NTPServers, snapshot.Interface
				}
				for _, value := range servers {
					if address, err := netip.ParseAddr(value); err == nil && address.IsLinkLocalUnicast() && address.Zone() == "" {
						value = address.WithZone(iface).String()
					}
					grant.Servers = append(grant.Servers, ntpconfig.LearnedServer{Server: ntpconfig.Server{Address: value, Port: 123}, ValidUntil: until})
				}
			}
		}
		r.mu.Unlock()
		localapi.JSON(w, 200, input)
	})
}
