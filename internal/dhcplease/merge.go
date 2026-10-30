// SPDX-License-Identifier: BSD-3-Clause

// Package dhcplease preserves dnsmasq lease files across migration and handoff.
package dhcplease

import (
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Merge preserves each unexpired binding and its absolute deadline. A newer
// removal excludes a binding; conflicting client or address ownership fails.
func Merge(a, b string, removed map[[2]string]bool, now time.Time) (string, error) {
	records := map[string][]string{}
	clients := map[string]string{}
	for _, data := range []string{a, b} {
		for _, line := range strings.Split(data, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "agent-info" || fields[0] == "vendorclass" {
				continue
			}
			if len(fields) != 5 {
				return "", errors.New("incomplete dnsmasq lease record")
			}
			if removed[[2]string{fields[1], fields[2]}] {
				continue
			}
			expires, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil || expires == 0 {
				return "", errors.New("DHCP handoff requires finite lease expiries")
			}
			if expires <= now.Unix() {
				continue
			}
			if _, err := net.ParseMAC(fields[1]); err != nil {
				return "", err
			}
			if ip := net.ParseIP(fields[2]); ip == nil || ip.To4() == nil {
				return "", errors.New("invalid DHCP lease address")
			}
			key := fields[4]
			if key == "*" {
				key = fields[1]
			}
			if previous := clients[key]; previous != "" && previous != fields[2] {
				return "", errors.New("conflicting DHCP client addresses")
			}
			clients[key] = fields[2]
			if old, ok := records[fields[2]]; ok {
				if old[1] != fields[1] || old[4] != fields[4] {
					return "", errors.New("conflicting DHCP address owners")
				}
				oldExpiry, _ := strconv.ParseInt(old[0], 10, 64)
				if oldExpiry > expires {
					continue
				}
			}
			records[fields[2]] = fields
		}
	}
	addresses := make([]string, 0, len(records))
	for address := range records {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	var lines strings.Builder
	for _, address := range addresses {
		lines.WriteString(strings.Join(records[address], " "))
		lines.WriteByte('\n')
	}
	return lines.String(), nil
}
