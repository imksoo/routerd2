package serviceconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/aftr"
)

// DSLite owns the complete dynamic B4 projection. References are service names,
// never arbitrary status paths. A DNS server must be reachable over the IPv6
// underlay before this tunnel exists.
type DSLite struct {
	Interface        string         `json:"interface"`
	Tunnel           string         `json:"tunnel"`
	LocalAddress     string         `json:"local_address,omitempty"`
	SourceFrom       string         `json:"source_from,omitempty"`
	SourceDelegation string         `json:"source_delegation,omitempty"`
	AFTRFrom         string         `json:"aftr_from,omitempty"`
	Candidates       []string       `json:"candidates,omitempty"`
	DNSFrom          []string       `json:"dns_from,omitempty"`
	Resolvers        []AFTRResolver `json:"resolvers,omitempty"`
	Selection        string         `json:"selection,omitempty"`
	Ordinal          int            `json:"ordinal,omitempty"`
	TimeoutSeconds   int            `json:"timeout_seconds,omitempty"`
	RetrySeconds     int            `json:"retry_seconds,omitempty"`
	B4Address        string         `json:"b4_address,omitempty"`
	// B4Peer explicitly requests point-to-point addressing. Omitting it keeps
	// the existing address-only behavior; it never implies a default route.
	B4Peer       string `json:"b4_peer,omitempty"`
	MTU          int    `json:"mtu,omitempty"`
	DefaultRoute *bool  `json:"default_route,omitempty"`
	RouteGateway string `json:"route_gateway,omitempty"`
	Table        int    `json:"table,omitempty"`
	Metric       int    `json:"metric,omitempty"`
}

type AFTRResolver struct {
	Address string `json:"address"`
	Port    uint16 `json:"port,omitempty"`
}

// The private instance includes dependency and kernel policy so changing either
// changes the immutable configuration digest, even though the resolver engine
// itself only needs Protocol.
type DSLiteInstance struct {
	Protocol aftr.Config `json:"protocol"`
	Intent   DSLite      `json:"intent"`
}

func (s Service) AFTRConfig() aftr.Config {
	c := s.DSLite
	return aftr.Config{Resource: s.Name, Interface: c.Interface, Selection: c.Selection, Ordinal: c.Ordinal, TimeoutSeconds: c.TimeoutSeconds, RetrySeconds: c.RetrySeconds}
}

func validateDSLite(s Service) error {
	s = Normalize(s)
	c := s.DSLite
	if !interfaceName.MatchString(c.Tunnel) || c.Tunnel == c.Interface {
		return errors.New("DS-Lite tunnel must be a distinct valid interface name")
	}
	if (c.SourceFrom == "") == (c.LocalAddress == "") {
		return errors.New("DS-Lite requires exactly one of source_from and local_address")
	}
	if c.SourceDelegation != "" && c.SourceFrom == "" {
		return errors.New("DS-Lite source_delegation requires a DHCPv6 source_from")
	}
	if len(c.Candidates) == 0 && c.AFTRFrom == "" {
		return errors.New("DS-Lite requires candidates or aftr_from")
	}
	if len(c.Candidates)+boolInt(c.AFTRFrom != "") > aftr.MaxCandidates || len(c.DNSFrom) > 8 || len(c.Resolvers) > aftr.MaxResolvers {
		return errors.New("DS-Lite candidate or DNS source limit exceeded")
	}
	if c.MTU != 0 && (c.MTU < 68 || c.MTU > 65495) || c.Table < 0 || uint64(c.Table) > 1<<32-1 || c.Metric < 0 || uint64(c.Metric) > 1<<32-1 {
		return errors.New("DS-Lite MTU/table/metric is invalid")
	}
	b4 := netip.MustParsePrefix("192.0.0.2/32")
	if c.B4Address != "" {
		p, err := netip.ParsePrefix(c.B4Address)
		if err != nil || !p.Addr().Is4() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || p.String() != c.B4Address {
			return errors.New("DS-Lite b4_address must be a canonical IPv4 host/prefix")
		}
		b4 = p
	}
	var peer netip.Prefix
	if c.B4Peer != "" {
		p, err := netip.ParsePrefix(c.B4Peer)
		if err != nil || !p.Addr().Is4() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || p.Bits() != 32 || p.String() != c.B4Peer || p.Addr() == b4.Addr() {
			return errors.New("DS-Lite b4_peer must be a canonical IPv4 unicast /32 distinct from b4_address")
		}
		if b4.Bits() != 32 {
			return errors.New("DS-Lite point-to-point b4_address must use /32 when b4_peer is configured")
		}
		peer = p
	}
	if c.RouteGateway != "" {
		gateway, err := netip.ParseAddr(c.RouteGateway)
		if err != nil || !gateway.Is4() || gateway.String() != c.RouteGateway || !peer.IsValid() || gateway != peer.Addr() {
			return errors.New("DS-Lite route_gateway must be the canonical IPv4 address of the explicit b4_peer")
		}
		if c.DefaultRoute != nil && !*c.DefaultRoute {
			return errors.New("DS-Lite route_gateway requires default_route to be enabled")
		}
	}
	config := s.AFTRConfig()
	config.ConfigDigest = strings.Repeat("0", 64)
	in := aftr.Input{Resource: s.Name, Interface: c.Interface, ConfigDigest: config.ConfigDigest, Generation: 1, UpdatedAt: time.Now().UTC(), Active: true, Source: aftr.Source{Address: c.LocalAddress}}
	for _, candidate := range c.Candidates {
		in.Candidates = append(in.Candidates, aftr.Candidate{Value: candidate, Origin: "configured"})
	}
	for _, resolver := range c.Resolvers {
		in.Resolvers = append(in.Resolvers, aftr.Resolver{Address: resolver.Address, Port: resolver.Port, Interface: c.Interface})
	}
	if err := aftr.ValidateInput(config, in); err != nil {
		return err
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validateDSLiteSources(services []Service) error {
	byName := map[string]Service{}
	tunnels := map[string]string{}
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, s := range services {
		if s.DSLite == nil {
			continue
		}
		c := s.DSLite
		if prior := tunnels[c.Tunnel]; prior != "" {
			return fmt.Errorf("DS-Lite services %s and %s share tunnel %s", prior, s.Name, c.Tunnel)
		}
		tunnels[c.Tunnel] = s.Name
		check := func(name, role string) error {
			other, ok := byName[name]
			if !ok || other.Interface() != c.Interface || role != "" && other.Role() != role || role == "" && other.Role() != "ra_client" && other.Role() != "dhcp6" {
				return fmt.Errorf("DS-Lite %s references unavailable or incompatible underlay service %q", s.Name, name)
			}
			return nil
		}
		if c.SourceFrom != "" {
			if err := check(c.SourceFrom, ""); err != nil {
				return err
			}
			source := byName[c.SourceFrom]
			if c.SourceDelegation == "" {
				if source.DHCP6 != nil && !source.DHCP6.RequestAddress {
					return fmt.Errorf("DS-Lite %s DHCPv6 source requires request_address or source_delegation", s.Name)
				}
			} else {
				found := false
				if source.DHCP6 != nil {
					for _, lan := range source.DHCP6.Delegations {
						found = found || lan.Name == c.SourceDelegation
					}
				}
				if !found {
					return fmt.Errorf("DS-Lite %s source_delegation must name a delegation of %s", s.Name, c.SourceFrom)
				}
			}
		}
		if c.AFTRFrom != "" {
			if err := check(c.AFTRFrom, "dhcp6"); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		for _, name := range c.DNSFrom {
			if seen[name] {
				return fmt.Errorf("DS-Lite %s duplicates DNS source %q", s.Name, name)
			}
			seen[name] = true
			if err := check(name, ""); err != nil {
				return err
			}
		}
	}
	for _, s := range services {
		if s.DSLite == nil {
			continue
		}
		if owner := tunnels[s.Interface()]; owner != "" {
			return fmt.Errorf("service %s cannot use dynamically owned DS-Lite tunnel of %s as its underlay", s.Name, owner)
		}
	}
	return nil
}
