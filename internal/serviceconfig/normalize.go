package serviceconfig

import (
	"net/netip"
	"slices"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/otlplogconfig"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

// Normalize owns the fields it changes. Operator IP spelling is not a network
// identity; compiled services and their references use the parsed address.
func Normalize(s Service) Service {
	address := func(v string) string {
		if a, err := netip.ParseAddr(v); err == nil {
			return a.String()
		}
		return v
	}
	if s.OTLPLogs != nil {
		c := otlplogconfig.Normalize(*s.OTLPLogs)
		s.OTLPLogs = &c
	}
	if s.Syslog != nil {
		c := syslogconfig.Normalize(*s.Syslog)
		s.Syslog = &c
	}
	if s.NTP != nil {
		c := *s.NTP
		c.Sources = slices.Clone(c.Sources)
		for i := range c.Sources {
			c.Sources[i].Servers = slices.Clone(c.Sources[i].Servers)
			for j := range c.Sources[i].Servers {
				server := &c.Sources[i].Servers[j]
				server.Address = address(server.Address)
				if server.Port == 0 {
					server.Port = 123
				}
			}
		}
		s.NTP = &c
	}
	if s.BFD != nil {
		c := bfd.Normalize(*s.BFD)
		s.BFD = &c
	}
	if s.DSLite != nil {
		c := *s.DSLite
		c.LocalAddress = address(c.LocalAddress)
		c.Candidates = slices.Clone(c.Candidates)
		for i, v := range c.Candidates {
			c.Candidates[i] = address(v)
		}
		c.Resolvers = slices.Clone(c.Resolvers)
		for i := range c.Resolvers {
			c.Resolvers[i].Address = address(c.Resolvers[i].Address)
		}
		s.DSLite = &c
	}
	return s
}
