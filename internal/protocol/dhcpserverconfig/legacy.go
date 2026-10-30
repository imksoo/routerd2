// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
)

// FromSpec converts the original public DHCP types after the owning compiler
// resolves sources, aliases, when, and separate DNS services. Seed supplies the
// actual interface, server_address, primary.prefix and private runtime paths.
// Old dnsmasq/kea/dhcpd process directives are not passed through to a shell.
func FromSpec(seed Config, s api.DHCPv4ServerSpec, reservations []api.DHCPv4ReservationSpec) (Config, error) {
	if s.Role != "" && s.Role != "server" {
		return Config{}, errors.New("native DHCP server does not implement transit role; use a relay module")
	}
	if s.Server != "" && s.Server != "dnsmasq" {
		return Config{}, errors.New("legacy kea/dhcpd backend semantics require explicit migration to native DHCP intent")
	}
	if s.LeaseFile != "" {
		return Config{}, errors.New("legacy leaseFile path is not reused; run routerd dhcp import-leases before startup")
	}
	if !reflect.DeepEqual(s.When, api.ResourceWhenSpec{}) {
		return Config{}, errors.New("DHCP when must be resolved by the service compiler")
	}
	if s.GatewayFrom.Resource != "" || s.DomainFrom.Resource != "" || len(s.DNSServerFrom) > 0 || len(s.NTPServerFrom) > 0 {
		return Config{}, errors.New("DHCP status value sources must be resolved before compiling native intent")
	}
	if s.DNS.Enabled || s.DNS.UpstreamSource != "" || s.DNS.UpstreamInterface != "" || len(s.DNS.UpstreamServers) > 0 || s.DNS.CacheSize != 0 {
		return Config{}, errors.New("embedded legacy DNS requires a separately compiled native DNS module")
	}
	if s.DNSSource == "dhcpv4" || s.DNSInterface != "" {
		return Config{}, errors.New("DHCP DNS interface sources must be resolved by the service compiler")
	}
	if s.Interface != "" && s.Interface != seed.Interface {
		return Config{}, errors.New("DHCP interface alias must be resolved before conversion")
	}
	for _, v := range s.ListenInterfaces {
		if v != seed.Interface {
			return Config{}, errors.New("multi-interface DHCP intent must be split into separately bound workers")
		}
	}
	c := Normalize(seed)
	c.Authoritative = s.Authoritative
	c.LogDHCP = s.LogDHCP
	c.StickyHoldDays = s.StickyHoldDays
	c.Primary.Start = first(s.AddressPool.Start, s.RangeStart, c.Primary.Start)
	c.Primary.End = first(s.AddressPool.End, s.RangeEnd, c.Primary.End)
	lease, e := ParseLeaseTime(first(s.AddressPool.LeaseTime, s.LeaseTime))
	if e != nil {
		return Config{}, e
	}
	c.Primary.LeaseSeconds = lease
	c.Primary.Router = s.Gateway
	// Preserve the original distinction: legacy scope-style fields default
	// router and DNS to self; direct service style uses explicit Gateway/DNS.
	scopeStyle := s.RouterSource != "" || s.Router != "" || s.DNSSource != "" || s.DNSInterface != "" || s.RangeStart != "" || s.RangeEnd != "" || s.Authoritative
	if scopeStyle {
		switch s.RouterSource {
		case "", "interfaceAddress":
			c.Primary.Router = c.ServerAddress
		case "static":
			if s.Router == "" {
				return Config{}, errors.New("static DHCP router source requires router")
			}
			c.Primary.Router = s.Router
		case "none":
			c.Primary.Router = ""
		default:
			return Config{}, errors.New("unsupported DHCP routerSource")
		}
		switch s.DNSSource {
		case "", "self":
			c.Primary.DNSServers = []string{c.ServerAddress}
		case "static":
			if len(s.DNSServers) == 0 {
				return Config{}, errors.New("static DHCP DNS source requires addresses")
			}
			c.Primary.DNSServers = append([]string(nil), s.DNSServers...)
		case "none":
			c.Primary.DNSServers = nil
		default:
			return Config{}, errors.New("unsupported DHCP dnsSource")
		}
	} else {
		c.Primary.DNSServers = append([]string(nil), s.DNSServers...)
	}
	c.Primary.NTPServers = append([]string(nil), s.NTPServers...)
	c.Primary.Domain = s.Domain
	c.Primary.Options = append([]Option(nil), s.Options...)
	profiles := map[string]api.DHCPv4ServerProfileSpec{}
	for _, p := range s.Profiles {
		if p.Name == "" || profiles[p.Name].Name != "" {
			return Config{}, errors.New("DHCP profile names must be unique")
		}
		profiles[p.Name] = p
	}
	c.Scopes = nil
	for _, scope := range s.Scopes {
		p := api.DHCPv4ServerProfileSpec{AddressPool: scope.AddressPool, Netmask: scope.Netmask, Gateway: scope.Gateway, DNSServers: scope.DNSServers, NTPServers: scope.NTPServers, Options: scope.Options}
		if scope.ProfileRef != "" {
			if !reflect.DeepEqual(p, api.DHCPv4ServerProfileSpec{}) {
				return Config{}, errors.New("DHCP profileRef cannot be combined with inline scope settings")
			}
			var ok bool
			p, ok = profiles[scope.ProfileRef]
			if !ok {
				return Config{}, errors.New("DHCP scope references an unknown profile")
			}
		}
		bits := 0
		base, e := netip.ParsePrefix(c.Primary.Prefix)
		if e != nil {
			return Config{}, errors.New("DHCP conversion requires primary.prefix")
		}
		bits = base.Bits()
		if p.Netmask != "" {
			ip := net.ParseIP(p.Netmask).To4()
			if ip == nil {
				return Config{}, errors.New("invalid DHCP scope netmask")
			}
			ones, total := net.IPMask(ip).Size()
			if total != 32 {
				return Config{}, errors.New("non-contiguous DHCP scope netmask")
			}
			bits = ones
		}
		start, e := netip.ParseAddr(p.AddressPool.Start)
		if e != nil || !start.Is4() {
			return Config{}, errors.New("DHCP scope requires a pool start")
		}
		l, e := ParseLeaseTime(p.AddressPool.LeaseTime)
		if e != nil {
			return Config{}, e
		}
		x := Scope{Name: scope.Name, Prefix: netip.PrefixFrom(start, bits).Masked().String(), Start: p.AddressPool.Start, End: p.AddressPool.End, LeaseSeconds: l, Router: p.Gateway, DNSServers: append([]string(nil), p.DNSServers...), NTPServers: append([]string(nil), p.NTPServers...), Domain: c.Primary.Domain, Options: append([]Option(nil), p.Options...), Match: scope.Match}
		c.Scopes = append(c.Scopes, x)
	}
	c.Reservations = nil
	for _, r := range reservations {
		if r.Scope != "" {
			return Config{}, errors.New("legacy DHCPv4Reservation.scope is unsupported upstream; resolve server and native scope explicitly")
		}
		if !reflect.DeepEqual(r.When, api.ResourceWhenSpec{}) {
			return Config{}, errors.New("DHCP reservation when must be resolved by the compiler")
		}
		l := 0
		if r.LeaseTime != "" {
			var e error
			l, e = ParseLeaseTime(r.LeaseTime)
			if e != nil {
				return Config{}, e
			}
		}
		c.Reservations = append(c.Reservations, Reservation{MACAddress: r.MACAddress, IPAddress: r.IPAddress, Hostname: r.Hostname, LeaseSeconds: l, Options: append([]Option(nil), r.Options...)})
	}
	c = Normalize(c)
	if e := Validate(c); e != nil {
		return Config{}, e
	}
	return c, nil
}

func ParseLeaseTime(s string) (int, error) {
	if s == "" {
		return DefaultLeaseSeconds, nil
	}
	if s == "infinite" {
		return 0, errors.New("native DHCP does not silently convert infinite leases to finite leases")
	}
	if n, e := strconv.ParseUint(s, 10, 32); e == nil {
		if n < 60 || n == uint64(^uint32(0)) {
			return 0, errors.New("DHCP lease duration must be 60..4294967294 seconds")
		}
		return int(n), nil
	}
	if strings.HasSuffix(s, "d") {
		n, e := strconv.ParseUint(strings.TrimSuffix(s, "d"), 10, 32)
		if e == nil && n > 0 && n <= 49710 {
			return int(n * 86400), nil
		}
	}
	d, e := time.ParseDuration(s)
	if e != nil || d < 60*time.Second || d%time.Second != 0 || uint64(d/time.Second) >= uint64(^uint32(0)) {
		return 0, fmt.Errorf("unsupported DHCP lease duration %q", s)
	}
	return int(d / time.Second), nil
}
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
