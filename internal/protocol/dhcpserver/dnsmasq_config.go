// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/lan/dhcpv4server"
)

// dnsmasqConfig uses routerd's serving configuration, rather than implementing
// another DHCP allocator or packet parser. Paths belong to the engine root.
func dnsmasqConfig(settings engineSettings) []byte {
	c := settings.Intent
	lines := []string{"port=0", "no-resolv", "no-hosts", "bind-dynamic"}
	if c.IngressInterface != "" {
		// Including both interfaces leaves the DHCP socket unpinned. dnsmasq
		// maps packets from the parent to the VIP before choosing a DHCP context.
		lines = append(lines, "bridge-interface="+c.Interface+","+c.IngressInterface)
		lines = append(lines, "interface="+c.IngressInterface)
	}
	lines = append(lines, "user=root", "group=root", "pid-file=/state/dnsmasq.pid", "dhcp-leasefile=/state/"+filepath.Base(c.StatePath), "log-facility=-", "dhcp-lease-max="+strconv.Itoa(c.MaxLeases))
	if c.StickyHoldDays > 0 || c.FingerprintRetention != nil {
		lines = append(lines, "dhcp-script=/usr/sbin/routerd-dhcp-event")
	}
	if c.FingerprintRetention != nil {
		lines = append(lines, "script-on-renewal")
	}
	if c.StickyHoldDays > 0 {
		lines = append(lines, "dhcp-hostsfile=/state/held.hosts")
	}
	if c.Authoritative {
		lines = append(lines, "dhcp-authoritative")
	}
	if c.LogDHCP {
		lines = append(lines, "log-dhcp")
	}
	// Keep the profile ranges before the catch-all, as routerd's active LAN
	// controller does. The selector tag and allocated-range tag are distinct.
	server := netip.MustParseAddr(c.ServerAddress)
	shared := make(map[netip.Prefix]bool)
	for _, scope := range append(append([]Scope(nil), c.Scopes...), c.Primary) {
		name := c.Resource + "-" + scope.Name
		tag := strings.NewReplacer("_", "-", ".", "-").Replace(name)
		selector := tag + "-match"
		for _, mac := range scope.Match.MACAddresses {
			lines = append(lines, "dhcp-mac=set:"+selector+","+strings.ToLower(mac))
		}
		for _, oui := range scope.Match.OUIPrefixes {
			lines = append(lines, "dhcp-mac=set:"+selector+","+strings.ToLower(oui)+":*:*:*")
		}
		rendered := dhcpv4server.RenderDnsmasqLines(dhcpv4server.Config{
			Name: name, IfName: c.Interface,
			AddressPool: api.DHCPAddressPoolSpec{Start: scope.Start, End: scope.End, LeaseTime: strconv.Itoa(scope.LeaseSeconds) + "s"},
			Gateway:     scope.Router, DNSServers: scope.DNSServers, NTPServers: scope.NTPServers, Domain: scope.Domain, Options: scope.Options,
		})
		prefix := netip.MustParsePrefix(scope.Prefix)
		// HA listens on the VIP interface; other LAN subnets remain on its
		// parent. Make those configured ranges available without moving addresses.
		if !prefix.Contains(server) && !shared[prefix] {
			lines = append(lines, "shared-network="+c.ServerAddress+","+scope.Start)
			shared[prefix] = true
		}
		mask := net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
		for _, line := range rendered {
			if strings.HasPrefix(line, "dhcp-range=") {
				at := strings.LastIndexByte(line, ',')
				line = line[:at] + "," + mask + line[at:]
				if len(scope.Match.MACAddresses)+len(scope.Match.OUIPrefixes) > 0 {
					line = strings.Replace(line, "dhcp-range=", "dhcp-range=tag:"+selector+",", 1)
				}
			}
			lines = append(lines, line)
		}
		// dnsmasq otherwise supplies itself as router and DNS server. Native
		// intent has already resolved whether either option is wanted.
		if scope.Router == "" && !hasOption(scope.Options, 3, "router") {
			lines = append(lines, "dhcp-option=tag:"+tag+",option:router")
		}
		if len(scope.DNSServers) == 0 && !hasOption(scope.Options, 6, "dns-server") {
			lines = append(lines, "dhcp-option=tag:"+tag+",option:dns-server")
		}
	}
	for i, reservation := range c.Reservations {
		tag := "reservation-" + strconv.Itoa(i)
		parts := []string{reservation.MACAddress, "set:" + tag}
		if reservation.Hostname != "" {
			parts = append(parts, reservation.Hostname)
		}
		parts = append(parts, reservation.IPAddress)
		if reservation.LeaseSeconds != 0 {
			parts = append(parts, strconv.Itoa(reservation.LeaseSeconds)+"s")
		}
		lines = append(lines, "dhcp-host="+strings.Join(parts, ","))
		for _, option := range reservation.Options {
			key := strconv.Itoa(option.Code)
			if option.Name != "" {
				key = "option:" + option.Name
			}
			lines = append(lines, fmt.Sprintf("dhcp-option=tag:%s,%s,%s", tag, key, option.Value))
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func hasOption(options []api.DHCPv4OptionSpec, code int, name string) bool {
	for _, option := range options {
		if option.Name != "" {
			if strings.EqualFold(option.Name, name) {
				return true
			}
		} else if option.Code == code {
			return true
		}
	}
	return false
}
