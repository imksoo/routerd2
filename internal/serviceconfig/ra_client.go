package serviceconfig

import (
	"fmt"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/raclient"
)

// RAClient is the WAN receive side. Kernel RA/autoconf is disabled on this
// interface by the sole network writer before the protocol process starts.
// Learned DNS remains typed protocol state; it never rewrites resolv.conf.
type RAClient struct {
	UseMTU           bool     `json:"use_mtu,omitempty"`
	UseHopLimit      bool     `json:"use_hop_limit,omitempty"`
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

func validateDHCPRA(services []Service) error {
	byName := make(map[string]Service, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, s := range services {
		if s.DHCP6 == nil || s.DHCP6.RAFrom == "" {
			continue
		}
		source := byName[s.DHCP6.RAFrom]
		if source.RAClient == nil || source.RAClient.Interface != s.DHCP6.Interface {
			return fmt.Errorf("DHCPv6 %s ra_from requires an RA client on the same interface", s.Name)
		}
	}
	return nil
}
