package runtimeplan

import "time"

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
		if !time.Now().Before(r.guardUntil) {
			return "applying", "supervisor status validity expired"
		}
		if err := r.checkAppliedLocked(); err != nil {
			return "applying", err.Error()
		}
	}
	return r.phase, r.lastError
}
