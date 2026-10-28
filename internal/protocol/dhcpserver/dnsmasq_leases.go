// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

// Adapted from routerd pkg/webconsole.readDnsmasqLeases. This observes the
// engine's file; it does not maintain another lease database or allocator.
func readDNSMasqLeases(c Config, now time.Time) ([]Lease, error) {
	data, err := os.ReadFile(c.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var leases []Lease
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		expires, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || expires <= now.Unix() {
			continue
		}
		mac, err := net.ParseMAC(fields[1])
		if err != nil {
			continue
		}
		lease := Lease{MACAddress: mac.String(), IPAddress: fields[2], ExpiresAt: time.Unix(expires, 0).UTC()}
		if fields[3] != "*" {
			lease.Hostname = fields[3]
		}
		if len(fields) > 4 && fields[4] != "*" {
			lease.ClientID = strings.ReplaceAll(fields[4], ":", "")
		}
		if !dhcpserverconfig.ValidHostname(lease.Hostname) {
			lease.Hostname = ""
		}
		leases = append(leases, lease)
	}
	return leases, nil
}
