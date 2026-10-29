package dnsconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// ListenSource is prepared once from listener intent. Interface addresses are
// observed by the DNS worker; delegated addresses come from applied PD state.
type ListenSource struct {
	Listener            int
	Interface           string
	Family              int
	Service, Delegation string
	VRRP                string
}

type ListenAddress struct {
	Source     int               `json:"source"`
	Address    netip.Addr        `json:"address"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

func ListenSources(c RuntimeConfig) ([]ListenSource, error) {
	var result []ListenSource
	for i, listener := range c.Spec.Listen {
		for _, ref := range listener.AddressFrom {
			parts := strings.Split(ref.Resource, "/")
			source := ListenSource{Listener: i}
			switch {
			case len(parts) == 2 && parts[0] == "Interface" && parts[1] != "":
				source.Interface = parts[1]
				if len(source.Interface) > 15 || strings.ContainsAny(source.Interface, "/\x00 \t\r\n") {
					return nil, errors.New("invalid DNS listener interface")
				}
				switch ref.Field {
				case "", "addresses":
				case "ipv4Addresses":
					source.Family = 4
				case "ipv6Addresses":
					source.Family = 6
				default:
					return nil, errors.New("DNS interface listener selects addresses, ipv4Addresses or ipv6Addresses")
				}
			case len(parts) == 2 && parts[0] == "VirtualAddress" && parts[1] != "":
				source.VRRP = parts[1]
				if ref.Field != "" && ref.Field != "address" {
					return nil, errors.New("DNS virtual listener selects address")
				}
			case len(parts) == 3 && parts[0] == "IPv6DelegatedAddress" && parts[1] != "" && parts[2] != "":
				source.Service, source.Delegation = parts[1], parts[2]
				if ref.Field != "" && ref.Field != "address" {
					return nil, errors.New("DNS delegated listener selects address")
				}
			default:
				return nil, fmt.Errorf("unsupported DNS listener source %q", ref.Resource)
			}
			result = append(result, source)
		}
	}
	return result, nil
}

func ValidateListenInput(sources []ListenSource, addresses []ListenAddress, clock boottime.Clock) error {
	if len(addresses) > 1024 {
		return errors.New("DNS input exceeds 1024 listen addresses")
	}
	for _, address := range addresses {
		if address.Source < 0 || address.Source >= len(sources) || sources[address.Source].Service == "" && sources[address.Source].VRRP == "" {
			return errors.New("DNS input has an unauthorized listener source")
		}
		ip := address.Address
		if ip.Is4In6() || !ip.IsGlobalUnicast() || sources[address.Source].Service != "" && !ip.Is6() {
			return errors.New("learned DNS listener requires an address of its configured family")
		}
		if err := address.ValidUntil.Validate(); err != nil {
			return err
		}
		if address.ValidUntil.BootID != clock.BootID {
			return errors.New("learned DNS listener requires its original boot deadline")
		}
	}
	return nil
}
