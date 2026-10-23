// SPDX-License-Identifier: BSD-3-Clause
package bgpconfig

import "time"

// Route is one selected, learned unicast path. Multiple entries for a prefix
// are an ECMP set, never a request for several competing Linux RouteReplace
// operations. The single kernel writer resolves global next hops recursively;
// link-local next hops require Interface. Locally originated paths are absent.
type Route struct {
	Prefix      string `json:"prefix"`
	Family      string `json:"family"`
	NextHop     string `json:"next_hop"`
	Interface   string `json:"interface,omitempty"`
	PeerAddress string `json:"peer_address"`
	Table       int    `json:"table"`
	Metric      int    `json:"metric"`
	Stale       bool   `json:"stale,omitempty"`
}

type Peer struct {
	Address      string `json:"address"`
	ASN          uint32 `json:"asn"`
	State        string `json:"state"`
	AdminState   string `json:"admin_state"`
	LocalAddress string `json:"local_address,omitempty"`
	RemotePort   uint32 `json:"remote_port,omitempty"`
}

// Snapshot is a complete replacement, not a route delta. Sequence advances
// for every publication including lease refreshes. An inactive snapshot
// withdraws all previously owned routes. The writer must fence old worker
// epochs and expire Active ownership at ValidUntil, including after SIGKILL.
type Snapshot struct {
	Resource      string    `json:"resource"`
	Epoch         string    `json:"epoch"`
	Sequence      uint64    `json:"sequence"`
	Active        bool      `json:"active"`
	ObservedAt    time.Time `json:"observed_at"`
	ValidUntil    time.Time `json:"valid_until,omitempty"`
	Routes        []Route   `json:"routes"`
	Peers         []Peer    `json:"peers"`
	RejectedPaths uint64    `json:"rejected_paths"`
	WatchEvents   uint64    `json:"watch_events"`
	Error         string    `json:"error,omitempty"`
}

// Event records bounded transition metadata. It intentionally omits passwords,
// full configuration and a copy of every RIB route.
type Event struct {
	Type     string    `json:"type"`
	Resource string    `json:"resource"`
	Epoch    string    `json:"epoch"`
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Active   bool      `json:"active"`
	Routes   int       `json:"routes"`
	Peers    int       `json:"peers"`
	Error    string    `json:"error,omitempty"`
}
