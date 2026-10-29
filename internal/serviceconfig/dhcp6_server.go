package serviceconfig

import (
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
)

type DHCP6Server struct {
	LogDHCP        bool           `json:"log_dhcp,omitempty"`
	Interface      string         `json:"interface"`
	ServerDUID     string         `json:"server_duid,omitempty"`
	DNSServers     []netip.Addr   `json:"dns_servers,omitempty"`
	SNTPServers    []netip.Addr   `json:"sntp_servers,omitempty"`
	DomainSearch   []string       `json:"domain_search,omitempty"`
	DNSFrom        []string       `json:"dns_from,omitempty"`
	DNSDelegation  *DNSDelegation `json:"dns_delegation,omitempty"`
	SNTPFrom       []string       `json:"sntp_from,omitempty"`
	SearchFrom     []string       `json:"search_from,omitempty"`
	RefreshSeconds uint32         `json:"refresh_seconds,omitempty"`
}

// DNSDelegation selects the router address installed from a PD allocation.
type DNSDelegation struct {
	Service    string `json:"service"`
	Delegation string `json:"delegation"`
}

type DHCP6ServerInput struct {
	ConfigDigest string                  `json:"config_digest"`
	Options      dhcp6serverconfig.Input `json:"options"`
}

func (s Service) DHCP6ServerConfig(stateDir string) dhcp6serverconfig.Config {
	c := s.DHCP6Server
	return dhcp6serverconfig.Config{LogDHCP: c.LogDHCP, Resource: s.Name, Interface: c.Interface, StatePath: filepath.Join(stateDir, "leases", s.Name+".dhcp6-server-duid"), ServerDUID: c.ServerDUID, DNSServers: c.DNSServers, SNTPServers: c.SNTPServers, DomainSearch: c.DomainSearch, RefreshSeconds: c.RefreshSeconds}
}

func validateDHCP6Servers(services []Service) error {
	byName := make(map[string]Service, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, s := range services {
		if s.DHCP6Server == nil {
			continue
		}
		if s.DHCP6Server.ServerDUID != "" {
			if _, err := dhcp6config.ParseDUID(s.DHCP6Server.ServerDUID); err != nil {
				return err
			}
		}
		if ref := s.DHCP6Server.DNSDelegation; ref != nil {
			provider := byName[ref.Service]
			found := false
			if provider.DHCP6 != nil {
				for _, allocation := range provider.DHCP6.Delegations {
					if allocation.Name == ref.Delegation && allocation.Interface == s.DHCP6Server.Interface && allocation.PrefixLength != 128 {
						found = true
					}
				}
			}
			if !found {
				return fmt.Errorf("DHCPv6 server %s DNS delegation must select a LAN allocation on its interface", s.Name)
			}
		}
		for _, sourceSet := range []struct {
			field string
			names []string
		}{
			{"dns_from", s.DHCP6Server.DNSFrom},
			{"sntp_from", s.DHCP6Server.SNTPFrom},
			{"search_from", s.DHCP6Server.SearchFrom},
		} {
			for _, name := range sourceSet.names {
				source := byName[name]
				if source.DHCP6 == nil && (sourceSet.field == "sntp_from" || source.RAClient == nil) {
					return fmt.Errorf("DHCPv6 server %s %s %s has no matching information provider", s.Name, sourceSet.field, name)
				}
			}
		}
	}
	return nil
}
