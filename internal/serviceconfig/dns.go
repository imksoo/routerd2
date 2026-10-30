package serviceconfig

import (
	"errors"
	"fmt"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

// DNSUpstreamOwner limits a learned endpoint to the named elected router.
// The endpoint retains the earlier of its protocol and election deadlines.
type DNSUpstreamOwner struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	VRRP     string `json:"vrrp"`
}

func validateDNSSources(services []Service) error {
	servers := map[string]dhcpserverconfig.Config{}
	for _, s := range services {
		if s.DHCPServer != nil {
			servers[s.Name] = s.DHCPServerConfig("/var/lib/routerd")
		}
	}
	for _, s := range services {
		if s.DNS == nil {
			continue
		}

		listeners, err := dnsconfig.ListenSources(s.DNS.RuntimeConfig)
		if err != nil {
			return err
		}
		recordSources, err := dnsconfig.RecordSources(s.DNS.RuntimeConfig)
		if err != nil {
			return err
		}
		for _, record := range recordSources {
			listeners = append(listeners, dnsconfig.ListenSource{VRRP: record.VRRP, Service: record.Service, Delegation: record.Delegation})
		}
		for _, ref := range listeners {
			if ref.VRRP != "" {
				found := false
				for _, provider := range services {
					found = found || provider.Name == ref.VRRP && provider.VRRP != nil
				}
				if !found {
					return fmt.Errorf("DNS service %s requires virtual listener %s", s.Name, ref.VRRP)
				}
			}
			if ref.Service == "" {
				continue
			}
			found := false
			for _, provider := range services {
				if provider.Name != ref.Service || provider.DHCP6 == nil {
					continue
				}
				for _, allocation := range provider.DHCP6.Delegations {
					if allocation.Name == ref.Delegation {
						found = true
					}
				}
			}
			if !found {
				return fmt.Errorf("DNS service %s requires delegated listener %s/%s", s.Name, ref.Service, ref.Delegation)
			}
		}

		for _, source := range s.DNS.Spec.Sources {
			for _, ref := range append(append([]api.StatusValueSourceSpec(nil), source.UpstreamFrom...), source.BootstrapResolverFrom...) {
				name, role, err := dnsconfig.UpstreamProvider(ref)
				if err != nil {
					return err
				}
				found := false
				for _, provider := range services {
					if provider.Name == name && provider.Role() == role {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("DNS service %s requires configured DNS provider %s", s.Name, name)
				}
			}
		}

		seenOwners := map[[2]string]bool{}
		for _, owner := range s.DNS.UpstreamOwners {
			key := [2]string{owner.Source, owner.Provider}
			if seenOwners[key] {
				return fmt.Errorf("DNS service %s has duplicate learned upstream ownership", s.Name)
			}
			seenOwners[key] = true
			foundSource, foundOwner := false, false
			for _, source := range s.DNS.Spec.Sources {
				if source.Name != owner.Source {
					continue
				}
				for _, ref := range append(append([]api.StatusValueSourceSpec(nil), source.UpstreamFrom...), source.BootstrapResolverFrom...) {
					name, _, _ := dnsconfig.UpstreamProvider(ref)
					foundSource = foundSource || name == owner.Provider
				}
			}
			for _, provider := range services {
				foundOwner = foundOwner || provider.Name == owner.VRRP && provider.VRRP != nil
			}
			if !foundSource || !foundOwner {
				return fmt.Errorf("DNS service %s upstream owner requires a declared learned provider and VRRP election", s.Name)
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
