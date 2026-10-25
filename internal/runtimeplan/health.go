package runtimeplan

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
	return HealthStatus{ProjectionGeneration: r.projectionGeneration, Revision: r.config.revision, Phase: r.phase, LastError: boundedText(r.lastError, 4096), LastErrorTruncated: len(r.lastError) > 4096, InstanceCount: len(r.bindings), RouteIssueCount: len(r.routeIssues)}
}
