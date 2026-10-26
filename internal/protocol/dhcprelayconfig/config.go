// Package dhcprelayconfig describes DHCPv4 relay intent without importing its engine.
package dhcprelayconfig

import (
	"errors"
	"net/netip"
	"regexp"
)

type Interface struct {
	Name    string   `json:"name"`
	Address string   `json:"address"` // local IPv4 address used as giaddr
	Servers []string `json:"servers"`
}

type Config struct {
	Interfaces []Interface `json:"interfaces"`
	VRF        string      `json:"vrf,omitempty"`
	MaxHops    int         `json:"max_hops,omitempty"` // zero selects four
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

func unicast(address string) bool {
	a, err := netip.ParseAddr(address)
	return err == nil && a.Is4() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLoopback() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

func Validate(c Config) error {
	if len(c.Interfaces) == 0 || len(c.Interfaces) > 32 || c.MaxHops < 0 || c.MaxHops > 16 {
		return errors.New("DHCP relay requires 1..32 interfaces, 1..8 servers and max_hops zero or 1..16")
	}
	if c.VRF != "" && !interfaceName.MatchString(c.VRF) {
		return errors.New("invalid DHCP relay VRF")
	}
	names, addresses := map[string]bool{}, map[string]bool{}
	for _, iface := range c.Interfaces {
		if !interfaceName.MatchString(iface.Name) || !unicast(iface.Address) || names[iface.Name] || addresses[iface.Address] {
			return errors.New("DHCP relay interfaces require distinct names and local unicast IPv4 addresses")
		}
		names[iface.Name], addresses[iface.Address] = true, true
	}
	for _, iface := range c.Interfaces {
		if len(iface.Servers) == 0 || len(iface.Servers) > 8 {
			return errors.New("DHCP relay requires 1..8 servers per interface")
		}
		servers := map[string]bool{}
		for _, server := range iface.Servers {
			if !unicast(server) || addresses[server] || servers[server] {
				return errors.New("DHCP relay servers must be distinct unicast IPv4 addresses outside its local addresses")
			}
			servers[server] = true
		}
	}
	return nil
}
