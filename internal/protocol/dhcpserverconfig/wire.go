// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import "time"

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

type Snapshot struct {
	Resource      string    `json:"resource"`
	Interface     string    `json:"interface"`
	ServerAddress string    `json:"server_address"`
	Epoch         string    `json:"epoch"`
	Sequence      uint64    `json:"sequence"`
	Active        bool      `json:"active"`
	ObservedAt    time.Time `json:"observed_at"`
	ValidUntil    time.Time `json:"valid_until"`
	Leases        []Lease   `json:"leases"`
	Stats         Stats     `json:"stats"`
	Error         string    `json:"error,omitempty"`
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
