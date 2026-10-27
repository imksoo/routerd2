package serviceconfig

import (
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
)

type DHCP6Server struct {
	Interface      string       `json:"interface"`
	ServerDUID     string       `json:"server_duid,omitempty"`
	DNSServers     []netip.Addr `json:"dns_servers,omitempty"`
	SNTPServers    []netip.Addr `json:"sntp_servers,omitempty"`
	DomainSearch   []string     `json:"domain_search,omitempty"`
	DNSFrom        []string     `json:"dns_from,omitempty"`
	SNTPFrom       []string     `json:"sntp_from,omitempty"`
	SearchFrom     []string     `json:"search_from,omitempty"`
	RefreshSeconds uint32       `json:"refresh_seconds,omitempty"`
}

type DHCP6ServerInput struct {
	ConfigDigest string                  `json:"config_digest"`
	Options      dhcp6serverconfig.Input `json:"options"`
}

func (s Service) DHCP6ServerConfig(stateDir string) dhcp6serverconfig.Config {
	c := s.DHCP6Server
	return dhcp6serverconfig.Config{Resource: s.Name, Interface: c.Interface, StatePath: filepath.Join(stateDir, "leases", s.Name+".dhcp6-server-duid"), ServerDUID: c.ServerDUID, DNSServers: c.DNSServers, SNTPServers: c.SNTPServers, DomainSearch: c.DomainSearch, RefreshSeconds: c.RefreshSeconds}
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
