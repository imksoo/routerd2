package resourceprogram

import (
	"errors"
	"net/netip"

	"github.com/imksoo/routerd2/internal/network"
)

// HostRoute selects locally originated IPv6 traffic and uses a preferred,
// installed global address from Interface. Bypasses keep delegated DS-Lite
// outer sources on their explicitly owned underlay routes.
type HostRoute struct {
	Interface   string            `json:"interface"`
	RouterFrom  string            `json:"router_from,omitempty"`
	Gateway     string            `json:"gateway"`
	Destination string            `json:"destination,omitempty"`
	Table       int               `json:"table"`
	Priority    int               `json:"priority"`
	Metric      int               `json:"metric,omitempty"`
	Bypasses    []HostRouteBypass `json:"bypasses,omitempty"`
}

type HostRouteBypass struct {
	Service  string `json:"service"`
	Priority int    `json:"priority"`
}

type HostRoutePlan struct {
	ID         string
	RouterFrom string
	Route      network.Route
	Rule       network.PolicyRule
	Bypasses   []HostRouteBypass
}

func prepareHostRoute(id string, spec HostRoute) (HostRoutePlan, error) {
	if spec.Destination == "" {
		spec.Destination = "::/0"
	}
	if spec.Metric == 0 {
		spec.Metric = 50
	}
	if (spec.Gateway == "") == (spec.RouterFrom == "") {
		return HostRoutePlan{}, errors.New("host route requires exactly one gateway or RA router source")
	}
	prefix, err := netip.ParsePrefix(spec.Destination)
	if err != nil || !prefix.Addr().Is6() || spec.Table == 0 {
		return HostRoutePlan{}, errors.New("host route requires an IPv6 destination and explicit routing table")
	}
	p := HostRoutePlan{ID: id, RouterFrom: spec.RouterFrom, Route: network.Route{Destination: spec.Destination, Interface: spec.Interface, Gateway: spec.Gateway, Table: spec.Table, Metric: spec.Metric}, Rule: network.PolicyRule{Family: 6, Priority: spec.Priority, Table: spec.Table, InInterface: "lo"}, Bypasses: spec.Bypasses}
	if err := network.Validate(network.Desired{Routes: []network.Route{p.Route}, PolicyRules: []network.PolicyRule{p.Rule}}); err != nil {
		return p, err
	}
	return p, nil
}
