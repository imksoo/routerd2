package serviceconfig

import (
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/raclient"
)

// RAClient is the WAN receive side. Kernel RA/autoconf is disabled on this
// interface by the sole network writer before the protocol process starts.
// Learned DNS remains typed protocol state; it never rewrites resolv.conf.
type RAClient struct {
	Interface        string   `json:"interface"`
	SourceAddress    string   `json:"source_address,omitempty"`
	InterfaceID      string   `json:"interface_id,omitempty"`
	SLAAC            *bool    `json:"slaac,omitempty"`
	DefaultRoute     *bool    `json:"default_route,omitempty"`
	OnLink           *bool    `json:"on_link,omitempty"`
	RouteInformation *bool    `json:"route_information,omitempty"`
	DNS              *bool    `json:"dns,omitempty"`
	AllowedRouters   []string `json:"allowed_routers,omitempty"`
	Table            int      `json:"table,omitempty"`
	Metric           int      `json:"metric,omitempty"`
}

func (s Service) RAClientConfig(stateDir string) raclient.Config {
	c := s.RAClient
	return raclient.Config{Resource: s.Name, Interface: c.Interface,
		StatePath:     filepath.Join(stateDir, "leases", s.Name+".ra.json"),
		SourceAddress: c.SourceAddress, InterfaceID: c.InterfaceID, SLAAC: c.SLAAC,
		DefaultRoute: c.DefaultRoute, OnLink: c.OnLink, RouteInformation: c.RouteInformation,
		DNS: c.DNS, AllowedRouters: append([]string(nil), c.AllowedRouters...),
	}
}
