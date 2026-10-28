// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

const ObservationLifetime = 5 * time.Second

// Lease is an active, locally durable binding. ClientID is the exact option 61
// value in hexadecimal, or empty when identity is the Ethernet chaddr.
// Hostname is validated before publication; it is not a shell or DNS command.
type Lease struct {
	ClientID   string    `json:"client_id,omitempty"`
	MACAddress string    `json:"mac_address"`
	IPAddress  string    `json:"ip_address"`
	Hostname   string    `json:"hostname,omitempty"`
	Scope      string    `json:"scope"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Reserved   bool      `json:"reserved,omitempty"`
}

type Stats struct {
	Received     uint64 `json:"received"`
	Rejected     uint64 `json:"rejected"`
	Offered      uint64 `json:"offered"`
	Acknowledged uint64 `json:"acknowledged"`
	NAK          uint64 `json:"nak"`
	Released     uint64 `json:"released"`
	Declined     uint64 `json:"declined"`
	Expired      uint64 `json:"expired"`
	Conflicts    uint64 `json:"conflicts"`
	Exhausted    uint64 `json:"exhausted"`
	Quarantined  int    `json:"quarantined"`
	Offers       int    `json:"offers"`
}

// SyncStatus reports the last standby acknowledgement, independently of local
// lease persistence and service availability.
type SyncStatus struct {
	AcknowledgedRevision uint64 `json:"acknowledged_revision"`
	AllocationBlocked    bool   `json:"allocation_blocked,omitempty"`
	Error                string `json:"error,omitempty"`
}

type Snapshot struct {
	Resource           string      `json:"resource"`
	Interface          string      `json:"interface"`
	ServerAddress      string      `json:"server_address"`
	Epoch              string      `json:"epoch"`
	Sequence           uint64      `json:"sequence"`
	Active             bool        `json:"active"`
	Standby            bool        `json:"standby,omitempty"`
	CheckpointRevision uint64      `json:"checkpoint_revision"`
	Sync               *SyncStatus `json:"sync,omitempty"`
	ObservedAt         time.Time   `json:"observed_at"`
	BootID             string      `json:"boot_id"`
	ObservedNanos      uint64      `json:"observed_nanos"`
	ExpiresNanos       uint64      `json:"expires_nanos"`
	Leases             []Lease     `json:"leases"`
	Stats              Stats       `json:"stats"`
	Error              string      `json:"error,omitempty"`
}

type Event struct {
	Type     string    `json:"type"`
	Resource string    `json:"resource"`
	Epoch    string    `json:"epoch"`
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	Lease    *Lease    `json:"lease,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// CurrentObservation uses the original publisher clock, including suspend.
// ObservedAt is display metadata; client binding expiry is checked separately.
func CurrentObservation(s Snapshot, clock boottime.Clock) bool {
	return s.BootID == clock.BootID && s.ObservedNanos > 0 && s.ObservedNanos <= uint64(clock.BoottimeNanoseconds) && uint64(clock.BoottimeNanoseconds) < s.ExpiresNanos
}
