package resourceprogram

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/imksoo/routerd2/internal/network"
)

// IPIP follows the address and original lease authority of one DHCPv4 client.
// Static endpoints use the ordinary Link and Address resources.
type IPIP struct {
	Name           string       `json:"name"`
	LocalFromDHCP4 string       `json:"local_from_dhcp4"`
	Remote         netip.Addr   `json:"remote"`
	Address        netip.Prefix `json:"address"`
	TTL            int          `json:"ttl,omitempty"`
	MTU            int          `json:"mtu,omitempty"`
}

type IPIPPlan struct {
	ID      string
	Source  string
	Link    network.Link
	Address netip.Prefix
	MTU     int
}

func prepareIPIP(id string, spec IPIP, resources []Resource) (IPIPPlan, string, error) {
	var provider *Resource
	for i := range resources {
		r := &resources[i]
		if r.Service != nil && r.Service.Name == spec.LocalFromDHCP4 && r.Service.DHCP4 != nil {
			provider = r
			break
		}
	}
	if provider == nil {
		return IPIPPlan{}, "", errors.New("IPIP local_from_dhcp4 must name a declared DHCPv4 service")
	}
	remote := spec.Remote.Unmap()
	if !remote.Is4() || remote.IsUnspecified() || remote.IsMulticast() || !spec.Address.IsValid() || !spec.Address.Addr().Is4() {
		return IPIPPlan{}, "", errors.New("IPIP requires an IPv4 remote endpoint and inner address/prefix")
	}
	// This partial link has no learned local endpoint yet. Its fixed attributes
	// are checked here; the network boundary validates the complete projection.
	if spec.Name == "lo" || spec.Name == provider.Service.Interface() || spec.TTL < 0 || spec.TTL > 255 {
		return IPIPPlan{}, "", fmt.Errorf("IPIP %s: invalid name or TTL", id)
	}
	if err := network.Validate(network.Desired{Interfaces: []network.Interface{{Name: spec.Name, MTU: spec.MTU}}}); err != nil {
		return IPIPPlan{}, "", err
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = 64
	}
	return IPIPPlan{ID: id, Source: provider.Service.ModuleName(), Link: network.Link{Name: spec.Name, Kind: "ipip", Parent: provider.Service.Interface(), Remote: remote.String(), TTL: ttl}, Address: spec.Address, MTU: spec.MTU}, provider.ID, nil
}
