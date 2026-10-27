package serviceconfig

import (
	"errors"
	"fmt"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

func validateDNSSources(services []Service) error {
	servers := map[string]dhcpserverconfig.Config{}
	for _, s := range services {
		if s.DHCPServer != nil {
			servers[s.Name] = s.DHCPServerConfig("/run/routerd", "/var/lib/routerd")
		}
	}
	for _, s := range services {
		if s.DNS == nil {
			continue
		}

		for _, source := range s.DNS.Spec.Sources {
			for _, ref := range source.UpstreamFrom {
				name, err := dnsconfig.UpstreamProvider(ref)
				if err != nil {
					return err
				}
				found := false
				for _, provider := range services {
					if provider.Name == name && provider.PPPoE != nil {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("DNS service %s requires configured PPP provider %s", s.Name, name)
				}
			}
		}
		sets, records := 0, 0
		for _, zone := range s.DNS.Zones {
			seen := map[string]bool{}
			for _, ref := range zone.Spec.DHCPDerived.Sources {
				name, valid := dnsconfig.LeaseSourceName(ref)
				server, found := servers[name]
				if !valid || !found || seen[name] || len(zone.Name) > 256 {
					return fmt.Errorf("DNS service %s zone %s requires distinct configured DHCP server sources; unresolved reference %q", s.Name, zone.Name, ref)
				}
				seen[name] = true
				sets++
				records += server.MaxLeases
			}
		}
		if sets > 128 || records > dnsconfig.MaxLeaseRecords {
			return errors.New("DNS lease sources exceed 128 source sets or 4096 possible records; reduce max_leases or split DNS instances")
		}
	}
	return nil
}
