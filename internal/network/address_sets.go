package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/miekg/dns"
)

// AddressSetSnapshot reports current membership observations for the applied
// revision. An unresolved set has a zero ResolvedAt; failed refreshes retain
// usable membership but report their error.
type AddressSetSnapshot struct {
	Revision uint64             `json:"revision"`
	Sets     []AddressSetStatus `json:"sets"`
}
type AddressSetStatus struct {
	Name       string       `json:"name"`
	Addresses  []netip.Addr `json:"addresses,omitempty"`
	ResolvedAt time.Time    `json:"resolved_at,omitzero"`
	Error      string       `json:"error,omitempty"`
}

func validateLocalRedirects(f *Firewall) error {
	sets := make(map[string]bool, len(f.AddressSets))
	for _, set := range f.AddressSets {
		if set.Name == "" || sets[set.Name] {
			return errors.New("address sets require distinct nonempty names")
		}
		sets[set.Name] = true
		for _, address := range set.Addresses {
			if !address.IsValid() || address.Zone() != "" {
				return fmt.Errorf("address set %s: invalid host address %q", set.Name, address)
			}
		}
		for _, name := range set.Names {
			if _, ok := dns.IsDomainName(dns.Fqdn(name)); name == "" || !ok {
				return fmt.Errorf("address set %s: invalid DNS name", set.Name)
			}
		}
		if set.RefreshInterval != "" {
			if interval, err := time.ParseDuration(set.RefreshInterval); err != nil || interval <= 0 {
				return fmt.Errorf("address set %s: refresh_interval must be a positive duration", set.Name)
			}
		}
		if len(set.Names) > 0 && len(set.Resolvers) == 0 {
			return fmt.Errorf("address set %s: named membership requires DNS resolver endpoints", set.Name)
		}
		for _, server := range set.Resolvers {
			host, port, err := net.SplitHostPort(server)
			n, portErr := strconv.Atoi(port)
			if _, addrErr := netip.ParseAddr(host); err != nil || portErr != nil || addrErr != nil || n < 1 || n > 65535 {
				return fmt.Errorf("address set %s: resolver must be an IP endpoint", set.Name)
			}
		}
	}
	for _, rule := range f.LocalRedirects {
		if !validName(rule.Interface) || !sets[rule.DestinationSet] || rule.Protocol != "tcp" && rule.Protocol != "udp" || rule.DestinationPort < 1 || rule.DestinationPort > 65535 || rule.Port < 1 || rule.Port > 65535 {
			return errors.New("local redirect requires an input interface, address set, tcp/udp and ports within 1..65535")
		}
	}
	return nil
}
