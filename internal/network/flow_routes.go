package network

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/lifetime"
)

// These are owned by the configured egress policies. The terminal rule runs
// immediately before Linux's default main lookup, after earlier route owners.
const (
	NoPathMark     uint32 = 0xffffffff
	NoPathPriority        = 32765
)

// FlowRoute assigns new forwarded IPv4 connections to a routing mark. Buckets
// contain one fixed mark or the prepared rendezvous map. No buckets gives an
// untracked packet NoPathMark. Earlier routes remain usable and the terminal
// rule prevents fallthrough to main. Established original-direction packets
// retain their conntrack mark across policy replacement.
type FlowRoute struct {
	Match               PacketMatch       `json:"match"`
	ExcludeDestinations []string          `json:"exclude_destinations,omitempty"`
	Buckets             []uint32          `json:"buckets"`
	Seed                uint32            `json:"seed,omitempty"`
	HashDestination     bool              `json:"hash_destination,omitempty"`
	Authority           lifetime.Deadline `json:"authority,omitzero"`
}

func validateFlowRoutes(f Firewall) error {
	if len(f.FlowRoutes) > 256 || len(f.FlowMarks) > 256 {
		return errors.New("flow routing exceeds 256 rules or marks")
	}
	marks := make(map[uint32]bool, len(f.FlowMarks))
	for _, mark := range f.FlowMarks {
		if mark == 0 || marks[mark] {
			return errors.New("flow marks must be distinct and nonzero")
		}
		marks[mark] = true
	}
	for _, route := range f.FlowRoutes {
		if err := validatePacketMatch(route.Match, "flow route"); err != nil {
			return err
		}
		if route.Match.Family != 4 || len(route.Buckets) != 0 && len(route.Buckets) != 1 && len(route.Buckets) != 256 {
			return errors.New("IPv4 flow routing requires one mark or 256 rendezvous buckets")
		}
		if err := route.Authority.Validate(); err != nil {
			return err
		}
		for _, mark := range route.Buckets {
			if !marks[mark] {
				return fmt.Errorf("flow mark %d has no owned routing mark", mark)
			}
		}
		for _, value := range route.ExcludeDestinations {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || !prefix.Addr().Is4() {
				return errors.New("flow exclusion requires an IPv4 prefix")
			}
		}
	}
	return nil
}
