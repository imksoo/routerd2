// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcplease

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/sys/unix"
)

// Import initializes an unused service directory from the stopped old server's
// dnsmasq file. It shares the worker lock and never grants HA ownership.
func Import(cfg dhcpserverconfig.Config, data []byte) (int, error) {
	if err := dhcpserverconfig.Validate(cfg); err != nil {
		return 0, err
	}
	cfg = dhcpserverconfig.Normalize(cfg)
	if len(data) > 8<<20 {
		return 0, errors.New("dnsmasq lease file exceeds 8 MiB")
	}
	// The old shared dnsmasq file can also contain its DHCPv6 identity and
	// IPv6 bindings. This operation initializes only the IPv4 server.
	var v4 strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "duid" {
			continue
		}
		if len(fields) == 5 {
			if ip, err := netip.ParseAddr(fields[2]); err == nil && ip.Is6() {
				continue
			}
		}
		v4.WriteString(line)
		v4.WriteByte('\n')
	}
	now := time.Now()
	leases, err := Merge(v4.String(), "", nil, now)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(leases, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ip := netip.MustParseAddr(fields[2])
		mac, _ := net.ParseMAC(fields[1])
		expires, _ := strconv.ParseInt(fields[0], 10, 64)
		clientID := fields[4]
		if clientID == "*" {
			clientID = ""
		}
		if err := dhcpserverconfig.ValidateLease(dhcpserverconfig.Lease{MACAddress: mac.String(), IPAddress: fields[2], ClientID: strings.ToLower(strings.ReplaceAll(clientID, ":", "")), ExpiresAt: time.Unix(expires, 0)}, now); err != nil {
			return 0, err
		}
		inside := netip.MustParsePrefix(cfg.Primary.Prefix).Contains(ip)
		for _, scope := range cfg.Scopes {
			inside = inside || netip.MustParsePrefix(scope.Prefix).Contains(ip)
		}
		if !inside || ip.String() == cfg.ServerAddress {
			return 0, errors.New("lease address is outside the server's declared client subnets")
		}
		for _, r := range cfg.Reservations {
			if r.IPAddress == ip.String() && !strings.EqualFold(r.MACAddress, fields[1]) || strings.EqualFold(r.MACAddress, fields[1]) && r.IPAddress != ip.String() {
				return 0, errors.New("lease conflicts with a declared reservation")
			}
		}
		count++
	}
	if count > cfg.MaxLeases {
		return 0, errors.New("imported bindings exceed the configured lease limit")
	}
	if count == 0 {
		return 0, errors.New("lease file contains no unexpired IPv4 bindings")
	}
	dir, name, err := statefs.OpenParent(cfg.StatePath, true)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return 0, err
	}
	lock := os.NewFile(uintptr(fd), "DHCP lease migration lock")
	defer lock.Close()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return 0, fmt.Errorf("DHCP service is running or another import owns its state: %w", err)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if entry.Name() != name+".lock" {
			return 0, errors.New("lease import requires an unused DHCP service state directory")
		}
	}
	f, err := os.CreateTemp(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), ".lease-import-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(leases); err != nil {
		f.Close()
		return 0, err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return 0, err
	}
	if err := unix.Renameat2(int(dir.Fd()), filepath.Base(f.Name()), int(dir.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return 0, err
	}
	return count, dir.Sync()
}
