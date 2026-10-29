package network

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/lifetime"
)

// FlowRoute assigns new forwarded IPv4 connections to a routing mark. Buckets
// contain one fixed mark or the prepared rendezvous map; no buckets blocks new
// connections when no path has current authority. Established original
// direction packets retain their conntrack mark across policy replacement.
type FlowRoute struct {
	Match               PacketMatch       `json:"match"`
	ExcludeDestinations []string          `json:"exclude_destinations,omitempty"`
	Buckets             []uint32          `json:"buckets"`
	Seed                uint32            `json:"seed,omitempty"`
	HashDestination     bool              `json:"hash_destination,omitempty"`
	Authority           lifetime.Deadline `json:"authority,omitzero"`
}

// FlowPath prevents an established marked connection from falling through a
// missing policy-table route to another WAN with the old translated source.
type FlowPath struct {
	Mark      uint32 `json:"mark"`
	Interface string `json:"interface"`
}

func validateFlowRoutes(f Firewall) error {
	if len(f.FlowRoutes) > 256 || len(f.FlowPaths) > 256 {
		return errors.New("flow routing exceeds 256 rules or paths")
	}
	marks := make(map[uint32]string)
	for _, path := range f.FlowPaths {
		if path.Mark == 0 || !validName(path.Interface) {
			return errors.New("flow path requires a mark and interface")
		}
		if previous, found := marks[path.Mark]; found && previous != path.Interface {
			return errors.New("one flow mark cannot select different interfaces")
		}
		marks[path.Mark] = path.Interface
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
			if _, found := marks[mark]; !found {
				return fmt.Errorf("flow mark %d has no owned output path", mark)
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
