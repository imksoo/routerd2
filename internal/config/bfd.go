package config

import (
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func validateBFDRouting(d Native) error {
	domains := network.RoutingDomains(d.Network)
	sources := map[string]serviceconfig.Service{}
	for _, service := range d.Services {
		if service.BFD == nil {
			continue
		}
		sources[service.Name] = service
		cfg := service.BFD
		if cfg.VRF != "" && domains[cfg.VRF].VRF != cfg.VRF {
			return fmt.Errorf("BFD %s requires declared VRF %s", service.Name, cfg.VRF)
		}
		owned := false
		for _, iface := range d.Network.Interfaces {
			if !cfg.Multihop && iface.Name != cfg.Interface || domains[iface.Name].VRF != cfg.VRF {
				continue
			}
			for _, address := range iface.Addresses {
				prefix, err := netip.ParsePrefix(address)
				if err == nil && prefix.Addr().String() == cfg.SourceAddress {
					owned = true
				}
			}
		}
		if !owned {
			return fmt.Errorf("BFD %s source_address must be committed in its interface/VRF scope", service.Name)
		}
	}
	for _, service := range d.Services {
		if service.BGP == nil {
			continue
		}
		for _, ref := range service.BGP.PeerBFD {
			source := sources[ref.Service].BFD // Full-source reference validation precedes this check.
			if source.VRF != "" {
				table := service.BGP.Table
				if table == 0 {
					table = 254
				}
				if table != domains[source.VRF].Table {
					return fmt.Errorf("BGP %s FIB table %d differs from BFD VRF %s table %d", service.Name, table, source.VRF, domains[source.VRF].Table)
				}
			}
		}
	}
	return nil
}
