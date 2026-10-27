package serviceconfig

import (
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

func (s Service) BFDConfig() bfd.Config {
	c := *s.BFD
	c.Resource = s.Name
	return bfd.Normalize(c)
}

func validateBFDReferences(services []Service) error {
	sources := map[string]Service{}
	for _, service := range services {
		if service.BFD != nil {
			sources[service.Name] = service
		}
	}
	for _, service := range services {
		if service.BGP == nil {
			continue
		}
		for address, ref := range service.BGP.PeerBFD {
			source, ok := sources[ref.Service]
			if !ok {
				return fmt.Errorf("BGP %s peer %s refers to unconfigured BFD service %s", service.Name, address, ref.Service)
			}
			if service.BGP.Runtime.Peers[address].EbgpMultihop > 1 && !source.BFD.Multihop {
				return fmt.Errorf("BGP %s peer %s cannot bind a multihop session to single-hop BFD", service.Name, address)
			}
			peer := ref.Address
			if peer == "" {
				peer = address
			}
			found := false
			bfdAddress, _ := netip.ParseAddr(peer)
			for _, configured := range source.BFD.Peers {
				configuredAddress, _ := netip.ParseAddr(configured.Address)
				found = found || configuredAddress == bfdAddress
			}
			if !found {
				return fmt.Errorf("BGP %s peer %s has no matching BFD peer %s in service %s", service.Name, address, peer, ref.Service)
			}
			bgpAddress, _ := netip.ParseAddr(address)
			if bgpAddress.Is4() != bfdAddress.Is4() {
				return fmt.Errorf("BGP %s peer %s must use a BFD session of the same address family", service.Name, address)
			}
			// The explicit dependency protects this exact BGP endpoint pair.
			// Monitoring an unrelated router requires a separate conditional
			// policy contract and must not silently become BFD session binding.
			if bfdAddress != bgpAddress {
				return fmt.Errorf("BGP %s BFD reference must monitor the BGP peer address", service.Name)
			}
			transport := service.BGP.PeerTransports[address]
			local, _ := netip.ParseAddr(transport.LocalAddress)
			sourceAddress, _ := netip.ParseAddr(source.BFD.SourceAddress)
			if local != sourceAddress {
				return fmt.Errorf("BGP %s BFD-bound peer %s requires explicit local_address %s", service.Name, address, source.BFD.SourceAddress)
			}
			binding := transport.BindInterface
			if binding == "" {
				binding = service.BGP.BindInterface
			}
			expected := source.BFD.Interface
			if source.BFD.Multihop {
				expected = source.BFD.VRF
			}
			if binding != expected {
				return fmt.Errorf("BGP %s BFD-bound peer %s requires bind_interface %q", service.Name, address, expected)
			}
		}
	}
	return nil
}
