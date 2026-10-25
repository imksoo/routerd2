package serviceconfig

import (
	"encoding/hex"
	"encoding/json"

	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

// Update fences a protocol snapshot to its immutable configuration and process
// generation and supervisor lifetime. Sequence is monotonic within ProducerEpoch.
// Ordinary publication acknowledges receipt, not kernel application. BGP and VRRP use
// separate application requests before advancing their protocol state.
type Update struct {
	SupervisorInstance string          `json:"supervisor_instance"`
	Name               string          `json:"name"`
	Role               string          `json:"role"`
	ConfigDigest       string          `json:"config_digest"`
	Generation         uint64          `json:"generation"`
	ProducerEpoch      string          `json:"producer_epoch"`
	Sequence           uint64          `json:"sequence"`
	Snapshot           json.RawMessage `json:"snapshot,omitempty"`
}

// ValidSupervisorInstance validates an opaque, nonpersistent process-tree ID.
// It fences restarts; it is not an authentication credential.
func ValidSupervisorInstance(value string) bool {
	b, err := hex.DecodeString(value)
	return len(value) == 32 && err == nil && len(b) == 16
}

// ObservationReceipt acknowledges only storage of a current observation.
type ObservationReceipt struct {
	Accepted bool `json:"accepted"`
}

// ApplicationReceipt acknowledges the coordinator's completed kernel projection.
type ApplicationReceipt struct {
	Applied bool          `json:"applied"`
	VRRP    vrrp.Decision `json:"vrrp,omitzero"`
}
