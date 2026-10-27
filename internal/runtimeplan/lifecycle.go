package runtimeplan

import (
	"context"
	"errors"
	"net/http"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/supervisor"
)

var errInactiveService = errors.New("service is not active")

// A child belongs to its prerequisite's lifetime even when its own file is
// unchanged. The compiled order puts prerequisites before their consumers.
func (r *Runtime) drainDependenciesLocked(ceiling map[string]supervisor.DynamicModule) {
	draining := make(map[string]bool)
	for _, id := range r.config.plan.Order {
		resource := r.config.resources[id]
		if resource.Service != nil {
			name := resource.Service.ModuleName()
			old := r.config.protocols.instances[name]
			desired, retained := ceiling[name]
			draining[id] = !retained || old.digest != desired.ConfigDigest
		}
		for _, dependency := range r.config.plan.Dependencies[id] {
			draining[id] = draining[id] || draining[dependency]
		}
		if draining[id] && resource.Service != nil {
			delete(ceiling, resource.Service.ModuleName())
		}
	}
}

func (r *Runtime) restart(ctx context.Context, name string) error {
	if err := r.op.Acquire(ctx, 1); err != nil {
		return err
	}
	defer r.op.Release(1)
	status, err := r.manager.Status(ctx)
	if err != nil {
		return err
	}
	if status.Lifecycle != supervisor.Running {
		return errors.New("supervisor is stopping")
	}
	r.mu.Lock()
	if _, active := r.bindings[name]; !active {
		r.mu.Unlock()
		return errInactiveService
	}
	ceiling := make(map[string]supervisor.DynamicModule, len(r.bindings))
	for name, bound := range r.bindings {
		ceiling[name] = supervisor.DynamicModule{Name: name, Role: bound.instance.Role, ConfigDigest: bound.digest}
	}
	delete(ceiling, name)
	r.drainDependenciesLocked(ceiling)
	r.mu.Unlock()
	if err := r.wait(ctx, ceiling); err != nil {
		return r.failure(err)
	}
	// Re-request the configured workers. Protocol acquisition and readiness
	// continue under the coordinator; this response acknowledges the request.
	if _, err := r.reconcileStep(ctx, nil); err != nil {
		return r.failure(err)
	}
	r.reportReadiness(ctx)
	return nil
}

func (r *Runtime) lifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/services/{name}/restart", func(w http.ResponseWriter, req *http.Request) {
		if err := r.restart(req.Context(), req.PathValue("name")); err != nil {
			code := http.StatusUnprocessableEntity
			if errors.Is(err, errInactiveService) {
				code = http.StatusNotFound
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, http.StatusAccepted, struct {
			Accepted bool `json:"accepted"`
		}{Accepted: true})
	})
}
