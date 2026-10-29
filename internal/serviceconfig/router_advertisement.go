package serviceconfig

import (
	"fmt"

	"github.com/imksoo/routerd2/internal/protocol/ra"
)

// RouterAdvertisement owns one LAN advertiser. From selects a delegated LAN;
// the remaining configuration is the immutable advertisement template.
type RouterAdvertisement struct {
	ra.Config
	From *RADelegation `json:"from,omitempty"`
}

type RADelegation struct {
	Service                  string  `json:"service"`
	Delegation               string  `json:"delegation"`
	RDNSSRouter              bool    `json:"rdnss_router,omitempty"`
	UseLearnedRDNSS          bool    `json:"use_learned_rdnss,omitempty"`
	UseLearnedDNSSL          bool    `json:"use_learned_dnssl,omitempty"`
	ValidLifetimeSeconds     *uint32 `json:"valid_lifetime_seconds,omitempty"`
	PreferredLifetimeSeconds *uint32 `json:"preferred_lifetime_seconds,omitempty"`
}

func validateAdvertisementSources(services []Service) error {
	for _, service := range services {
		if service.RA == nil || service.RA.From == nil {
			continue
		}
		ref := service.RA.From
		found := false
		for _, provider := range services {
			if provider.Name != ref.Service || provider.DHCP6 == nil {
				continue
			}
			for _, allocation := range provider.DHCP6.Delegations {
				if allocation.Name == ref.Delegation && allocation.Interface == service.RA.Interface && allocation.PrefixLength != 128 {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("RA service %s must select a delegated LAN on its interface", service.Name)
		}
		if ref.ValidLifetimeSeconds != nil && ref.PreferredLifetimeSeconds != nil && *ref.PreferredLifetimeSeconds > *ref.ValidLifetimeSeconds {
			return fmt.Errorf("RA service %s preferred lifetime exceeds valid lifetime", service.Name)
		}
	}
	return nil
}
