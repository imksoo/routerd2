package serviceconfig

import (
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

// BGP keeps protocol intent public and all file/socket paths runtime-owned.
// FIBInterface is an explicit egress hint, not a VRF binding. Learned next hops
// are resolved through committed connected/static/learned routing intent.
type BGP struct {
	Runtime                   bgpconfig.AppliedConfig            `json:"runtime"`
	PeerTransports            map[string]bgpconfig.PeerTransport `json:"peer_transports,omitempty"`
	FIBInterface              string                             `json:"fib_interface,omitempty"`
	BindInterface             string                             `json:"bind_interface,omitempty"`
	Table                     int                                `json:"table,omitempty"`
	RouteMetric               int                                `json:"route_metric,omitempty"`
	MaxRoutes                 int                                `json:"max_routes,omitempty"`
	EffectTimeoutMilliseconds int                                `json:"effect_timeout_milliseconds,omitempty"`
}

func (s Service) BGPConfig(runtimeDir, stateDir string) bgpconfig.Config {
	c := s.BGP
	maxRoutes := c.MaxRoutes
	if maxRoutes == 0 {
		maxRoutes = 4096
	}
	private := filepath.Join(runtimeDir, "services", s.Name)
	return bgpconfig.Normalize(bgpconfig.Config{Resource: s.Name, Runtime: c.Runtime, PeerTransports: c.PeerTransports, FIBInterface: c.FIBInterface, BindInterface: c.BindInterface, Table: c.Table, RouteMetric: c.RouteMetric, MaxRoutes: maxRoutes, EffectTimeoutMilliseconds: c.EffectTimeoutMilliseconds, SocketPath: filepath.Join(private, "bgp.sock"), GRPCSocketPath: filepath.Join(private, "bgp-grpc.sock"), StatePath: filepath.Join(stateDir, "services", s.Name, "bgp.json"), StatusPath: filepath.Join(private, "status.json"), EventPath: filepath.Join(stateDir, "services", s.Name, "bgp-events.jsonl")})
}
