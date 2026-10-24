package health

import (
	"fmt"

	old "github.com/imksoo/routerd/pkg/healthcheck"
)

func ScalarFieldSupported(field string) bool {
	switch field {
	case "active", "phase", "lastResult", "lastCheckedAt", "lastTransitionAt", "consecutivePassed", "consecutiveFailed", "failureCount", "message", "lastSuccessTime", "lastFailureTime", "firstFailureTime":
		return true
	default:
		return false
	}
}

// ScalarStatus preserves the pinned HealthCheck scalar representation. An
// observed empty optional field is known empty, distinct from no live sample.
func ScalarStatus(s Snapshot, field string) (string, bool) {
	if !ScalarFieldSupported(field) {
		return "", false
	}
	if field == "active" {
		return fmt.Sprint(s.Active), true
	}
	value, exists := old.StatusMap(s.State)[field]
	if !exists {
		return "", true
	}
	return fmt.Sprint(value), true
}
