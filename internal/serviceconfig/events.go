package serviceconfig

import "encoding/json"

// Update fences a protocol snapshot to its immutable configuration and process
// generation. Sequence is monotonic within ProducerEpoch; an acknowledged
// duplicate has no second effect. Snapshots are intent, never shell commands.
type Update struct {
	Name          string          `json:"name"`
	Role          string          `json:"role"`
	ConfigDigest  string          `json:"config_digest"`
	Generation    uint64          `json:"generation"`
	ProducerEpoch string          `json:"producer_epoch"`
	Sequence      uint64          `json:"sequence"`
	Snapshot      json.RawMessage `json:"snapshot"`
}
