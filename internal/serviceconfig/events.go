package serviceconfig

import (
	"encoding/hex"
	"encoding/json"
)

// Update fences a protocol snapshot to its immutable configuration and process
// generation and supervisor lifetime. Sequence is monotonic within ProducerEpoch; an acknowledged
// duplicate has no second effect. Snapshots are intent, never shell commands.
type Update struct {
	SupervisorInstance string          `json:"supervisor_instance"`
	Name               string          `json:"name"`
	Role               string          `json:"role"`
	ConfigDigest       string          `json:"config_digest"`
	Generation         uint64          `json:"generation"`
	ProducerEpoch      string          `json:"producer_epoch"`
	Sequence           uint64          `json:"sequence"`
	Snapshot           json.RawMessage `json:"snapshot"`
}

// ValidSupervisorInstance validates an opaque, nonpersistent process-tree ID.
// It fences restarts; it is not an authentication credential.
func ValidSupervisorInstance(value string) bool {
	b, err := hex.DecodeString(value)
	return len(value) == 32 && err == nil && len(b) == 16
}
