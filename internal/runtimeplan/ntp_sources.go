package runtimeplan

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
)

func (r *Runtime) ntpSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/ntp-sources", func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		name := request.PathValue("name")
		node, configured := r.config.protocols.ntp[name]
		binding, selected := r.bindings[name]
		if !configured || !selected {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusNotFound, errors.New("NTP service is not selected"))
			return
		}
		producer := r.guardStatus[name]
		if producer.Generation == 0 || producer.ConfigDigest != binding.digest {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("NTP process authority is unknown"))
			return
		}
		input := ntpconfig.Sources{ConfigDigest: binding.digest, Generation: producer.Generation}
		clock, err := boottime.Read()
		if err != nil {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusServiceUnavailable, err)
			return
		}
		if !time.Now().Before(r.guardUntil) {
			r.mu.Unlock()
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("NTP source process observation is stale"))
			return
		}
		for _, ref := range node.SourcesFrom {
			moduleName := "service." + ref
			owner, active := r.bindings[moduleName]
			module := r.guardStatus[moduleName]
			record := r.records[moduleName]
			snapshot := r.guardGrants[moduleName].dhcp6
			if !active || snapshot == nil || !module.Ready || module.Generation != record.Generation || module.ConfigDigest != owner.digest || record.ConfigDigest != owner.digest || snapshot.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
				continue
			}
			until, live := snapshot.InformationUntil(clock)
			if !live {
				continue
			}
			for _, value := range snapshot.SNTPServers {
				if address, err := netip.ParseAddr(value); err == nil && address.IsLinkLocalUnicast() && address.Zone() == "" {
					value = address.WithZone(snapshot.Interface).String()
				}
				input.Servers = append(input.Servers, ntpconfig.LearnedServer{Server: ntpconfig.Server{Address: value, Port: 123}, ValidUntil: until})
			}
		}
		r.mu.Unlock()
		localapi.JSON(w, http.StatusOK, input)
	})
}
