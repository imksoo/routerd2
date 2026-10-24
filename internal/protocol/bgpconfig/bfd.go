// SPDX-License-Identifier: BSD-3-Clause

package bgpconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

const MaxBFDPeers = 64
const MaxBFDDetectionNanoseconds = int64(255) * int64(^uint32(0)) * 1000

// BFDReference explicitly gates one configured BGP peer on an exact native BFD
// service/peer. This does not reinterpret upstream AppliedPeer.BFD profiles.
type BFDReference struct {
	Service string `json:"service"`
	Address string `json:"address,omitempty"`
}

// BFDProjection is scoped to the recipient BGP worker. Grant identities refer
// to their source BFD workers. A missing peer grants no health.
type BFDProjection struct {
	ConfigDigest string              `json:"config_digest"`
	Generation   uint64              `json:"generation"`
	Peers        map[string]BFDGrant `json:"peers"`
}

type BFDGrant struct {
	Service                     string `json:"service"`
	Address                     string `json:"address"`
	ConfigDigest                string `json:"config_digest,omitempty"`
	Generation                  uint64 `json:"generation,omitempty"`
	ProducerEpoch               string `json:"producer_epoch,omitempty"`
	Sequence                    uint64 `json:"sequence,omitempty"`
	BootID                      string `json:"boot_id,omitempty"`
	LastReceivedBootNanoseconds int64  `json:"last_received_boot_nanoseconds,omitempty"`
	DeadlineBootNanoseconds     int64  `json:"deadline_boot_nanoseconds,omitempty"`
	DownTransitions             uint64 `json:"down_transitions"`
	Up                          bool   `json:"up"`
}

// BFDAcknowledgement proves that the BGP gate completed an actual peer-admin
// transition for this BFD source epoch/down counter. Route consumers must also
// check current finite BFD health; this proof alone is never a health grant.
type BFDAcknowledgement struct {
	ConfigDigest    string `json:"config_digest,omitempty"`
	Generation      uint64 `json:"generation,omitempty"`
	ProducerEpoch   string `json:"producer_epoch,omitempty"`
	DownTransitions uint64 `json:"down_transitions"`
	Enabled         bool   `json:"enabled"`
}

var bfdServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
var bfdDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var bfdEpoch = regexp.MustCompile(`^[0-9a-f]{32}$`)
var bfdBootID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidateBFDReferences(c Config) error {
	if len(c.PeerBFD) > MaxBFDPeers {
		return errors.New("BGP peer_bfd is limited to 64 peers per worker")
	}
	for peer, ref := range c.PeerBFD {
		configured, exists := c.Runtime.Peers[peer]
		address, err := netip.ParseAddr(peer)
		if !exists || configured.Address != peer || err != nil || address.String() != peer {
			return fmt.Errorf("BFD reference has no exact configured BGP peer %q", peer)
		}
		if !bfdServiceName.MatchString(ref.Service) {
			return errors.New("BGP BFD service reference must be a service name")
		}
		if configured.EbgpMultihop > 1 {
			return errors.New("native BFD-bound BGP peers do not support multihop")
		}
		if c.Runtime.Global.GracefulRestart != nil && c.Runtime.Global.GracefulRestart.Enabled || configured.GracefulRestart != nil && configured.GracefulRestart.Enabled {
			return errors.New("native BFD-bound BGP peers cannot enable graceful restart; immediate withdrawal is required")
		}
		if ref.Address == "" {
			ref.Address = peer
		}
		remote, err := netip.ParseAddr(ref.Address)
		if err != nil || remote.String() != ref.Address || remote.Zone() != "" || remote.Is4In6() || remote.IsUnspecified() || remote.IsMulticast() || remote.IsLoopback() || remote == netip.MustParseAddr("255.255.255.255") || remote.Is4() != address.Is4() {
			return errors.New("BGP BFD address must be a canonical unicast address of the BGP peer family")
		}
	}
	return nil
}

func ValidateBFDProjection(c Config, p BFDProjection, digest string, generation uint64) error {
	if !bfdDigest.MatchString(digest) || generation == 0 || p.ConfigDigest != digest || p.Generation != generation {
		return errors.New("BFD projection belongs to a different BGP worker binding")
	}
	if len(c.PeerBFD) == 0 || len(c.PeerBFD) > MaxBFDPeers || len(p.Peers) > len(c.PeerBFD) {
		return errors.New("BFD projection has an invalid bounded peer scope")
	}
	for peer, grant := range p.Peers {
		ref, exists := c.PeerBFD[peer]
		if ref.Address == "" {
			ref.Address = peer
		}
		if !exists || grant.Service != ref.Service || grant.Address != ref.Address {
			return errors.New("BFD projection crosses the configured peer reference")
		}
		if grant.ConfigDigest != "" && !bfdDigest.MatchString(grant.ConfigDigest) || grant.ProducerEpoch != "" && !bfdEpoch.MatchString(grant.ProducerEpoch) || grant.BootID != "" && !bfdBootID.MatchString(grant.BootID) || grant.LastReceivedBootNanoseconds < 0 || grant.DeadlineBootNanoseconds < 0 {
			return errors.New("BFD grant has malformed source metadata")
		}
		if grant.DeadlineBootNanoseconds != 0 && (grant.LastReceivedBootNanoseconds <= 0 || grant.DeadlineBootNanoseconds <= grant.LastReceivedBootNanoseconds || grant.DeadlineBootNanoseconds-grant.LastReceivedBootNanoseconds > MaxBFDDetectionNanoseconds) {
			return errors.New("BFD grant contains an unbounded detection deadline")
		}
		if grant.Up && (!bfdDigest.MatchString(grant.ConfigDigest) || grant.Generation == 0 || !bfdEpoch.MatchString(grant.ProducerEpoch) || grant.Sequence == 0 || !bfdBootID.MatchString(grant.BootID) || grant.LastReceivedBootNanoseconds <= 0 || grant.DeadlineBootNanoseconds <= grant.LastReceivedBootNanoseconds) {
			return errors.New("BFD Up grant lacks complete finite source evidence")
		}
	}
	return nil
}
