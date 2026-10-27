package serviceconfig

import (
	"net/netip"
	"slices"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
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
