package runtimeplan

import "github.com/imksoo/routerd2/internal/boottime"

// HealthStatus is intentionally independent of protocol snapshot sizes. Health
// polling neither encodes instance snapshots nor copies route-issue details.
type HealthStatus struct {
	ProjectionGeneration uint64 `json:"projection_generation"`
	Revision             uint64 `json:"revision"`
	Phase                string `json:"phase"`
	LastError            string `json:"last_error,omitempty"`
	LastErrorTruncated   bool   `json:"last_error_truncated,omitempty"`
	InstanceCount        int    `json:"instance_count"`
	RouteIssueCount      int    `json:"route_issue_count"`
}

func (r *Runtime) Health() HealthStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	phase, lastError := r.phaseLocked()
	return HealthStatus{ProjectionGeneration: r.projectionGeneration, Revision: r.config.revision, Phase: phase, LastError: boundedText(lastError, 4096), LastErrorTruncated: len(lastError) > 4096, InstanceCount: len(r.bindings), RouteIssueCount: len(r.routeIssues)}
}

func (r *Runtime) phaseLocked() (string, string) {
	if r.phase == "ready" {
		if !r.guard.ready {
			return "applying", "required routerd modules are not ready"
		}
		clock, err := boottime.Read()
		if err != nil {
			return "applying", err.Error()
		}
		if r.guard.bootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= r.guard.untilNanos {
			return "applying", "supervisor status validity expired"
		}
		if err := r.checkAppliedLocked(); err != nil {
			return "applying", err.Error()
		}
	}
	return r.phase, r.lastError
}
