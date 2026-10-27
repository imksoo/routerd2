package runtimeplan

import (
	"errors"
	"net/http"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
)

func (r *Runtime) tailscaleInput(name string) (tailscaleconfig.Input, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[name]
	if !ok || binding.instance.Role != "tailscale" {
		return tailscaleconfig.Input{}, errors.New("no configured Tailscale node")
	}
	clock, err := boottime.Read()
	if err != nil {
		return tailscaleconfig.Input{}, err
	}
	out := tailscaleconfig.Input{ConfigDigest: binding.digest, Authority: lifetime.Deadline{BootID: clock.BootID, Nanos: uint64(clock.BoottimeNanoseconds)}}
	if r.applied.sourceEpoch != r.sourceEpoch {
		return out, nil
	}
	for id, resource := range r.config.resources {
		if resource.Service == nil || resource.Service.ModuleName() != name {
			continue
		}
		if state := r.resourceStates[id]; state.Active {
			out.Authority = state.ActiveUntil
			if out.Authority == (lifetime.Deadline{}) {
				out.Authority.BootID = clock.BootID
			}
		}
		break
	}
	return out, nil
}
func (r *Runtime) tailscaleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/tailscale-input", func(w http.ResponseWriter, req *http.Request) {
		input, err := r.tailscaleInput(req.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 404, err)
			return
		}
		localapi.JSON(w, 200, input)
	})
}
