package dnsconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// RecordSource binds a configured record to its current address owner.
type RecordSource struct {
	Zone, Record              int
	IPv6                      bool
	Service, Delegation, VRRP string
}

type RecordAddress struct {
	Zone       int               `json:"zone"`
	Record     int               `json:"record"`
	Address    netip.Addr        `json:"address"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

func RecordSources(c RuntimeConfig) ([]RecordSource, error) {
	var out []RecordSource
	for z, zone := range c.Zones {
		for r, record := range zone.Spec.Records {
			for family, ref := range []api.StatusValueSourceSpec{record.IPv4From, record.IPv6From} {
				literal := record.IPv4
				if family == 1 {
					literal = record.IPv6
				}
				if ref == (api.StatusValueSourceSpec{}) {
					continue
				}
				if literal != "" || ref.Optional || ref.Field != "" && ref.Field != "address" {
					return nil, errors.New("DNS address source requires one owner and its address field")
				}
				source := RecordSource{Zone: z, Record: r, IPv6: family == 1}
				parts := strings.Split(ref.Resource, "/")
				switch {
				case len(parts) == 2 && parts[0] == "VirtualAddress" && parts[1] != "":
					source.VRRP = parts[1]
				case family == 1 && len(parts) == 3 && parts[0] == "IPv6DelegatedAddress" && parts[1] != "" && parts[2] != "":
					source.Service, source.Delegation = parts[1], parts[2]
				default:
					return nil, fmt.Errorf("unsupported DNS record address owner %q", ref.Resource)
				}
				out = append(out, source)
			}
		}
	}
	return out, nil
}

func validateRecordInput(c RuntimeConfig, addresses []RecordAddress, clock boottime.Clock) error {
	if len(addresses) > 4096 {
		return errors.New("DNS record input exceeds 4096 addresses")
	}
	type key struct {
		zone, record int
		address      netip.Addr
	}
	seen := map[key]bool{}
	for _, address := range addresses {
		family := 0
		if address.Address.Is6() {
			family = 1
		}
		identity := key{address.Zone, address.Record, address.Address}
		if seen[identity] || address.Address.Is4In6() || !address.Address.IsGlobalUnicast() {
			return errors.New("DNS record input requires distinct unicast addresses of the configured family")
		}
		seen[identity] = true
		if address.Zone < 0 || address.Zone >= len(c.Zones) || address.Record < 0 || address.Record >= len(c.Zones[address.Zone].Spec.Records) {
			return errors.New("DNS record input has no configured record")
		}
		declared := c.Zones[address.Zone].Spec.Records[address.Record]
		owner := declared.IPv4From
		if family == 1 {
			owner = declared.IPv6From
		}
		if owner.Resource == "" {
			return errors.New("DNS record input has no configured address owner")
		}
		if err := address.ValidUntil.Validate(); err != nil {
			return err
		}
		if address.ValidUntil.BootID != clock.BootID {
			return errors.New("DNS record input requires its original owner boot deadline")
		}
	}
	return nil
}
