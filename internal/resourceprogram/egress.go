package resourceprogram

import (
	"errors"
	"fmt"
	"math"

	"github.com/imksoo/routerd2/internal/network"
)

// EgressRoute owns one kernel route. Among currently eligible candidates, the
// highest preference wins; equal preferences share the route using kernel ECMP.
// NextHop.Weight sets the relative distribution within that selected group.
type EgressRoute struct {
	Destination string            `json:"destination"`
	Table       int               `json:"table,omitempty"`
	Metric      int               `json:"metric,omitempty"`
	Candidates  []EgressCandidate `json:"candidates"`
}

type EgressCandidate struct {
	Preference int             `json:"preference"`
	When       *FactCondition  `json:"when,omitempty"`
	NextHop    network.NextHop `json:"next_hop"`
}

func (e EgressRoute) route() network.Route {
	return network.Route{Destination: e.Destination, Table: e.Table, Metric: e.Metric}
}

func (e EgressRoute) compile() (network.Route, error) {
	if len(e.Candidates) == 0 {
		return network.Route{}, errors.New("egress route requires candidates")
	}
	route := e.route()
	for i, candidate := range e.Candidates {
		if candidate.When != nil {
			if err := ValidateFactCondition(*candidate.When); err != nil {
				return network.Route{}, fmt.Errorf("candidate %d: %w", i, err)
			}
		}
		route.NextHops = append(route.NextHops, candidate.NextHop)
	}
	return route, nil
}

func (e EgressRoute) selectRoute(facts FactSnapshot) (network.Route, uint64, bool) {
	route := e.route()
	until := uint64(math.MaxUint64)
	preference, selected := 0, false
	for _, candidate := range e.Candidates {
		value, deadline := True, uint64(math.MaxUint64)
		if candidate.When != nil {
			value, deadline = facts.ConditionAuthority(*candidate.When)
		}
		if value != True || selected && candidate.Preference < preference {
			continue
		}
		if !selected || candidate.Preference > preference {
			route.NextHops = route.NextHops[:0]
			until = math.MaxUint64
			preference, selected = candidate.Preference, true
		}
		route.NextHops = append(route.NextHops, candidate.NextHop)
		until = min(until, deadline)
	}
	return route, until, selected
}
