// SPDX-License-Identifier: BSD-3-Clause

// Package conntrackconfig describes a conntrackd FTFW replication peer.
package conntrackconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

type Config struct {
	Interface    string       `json:"interface"`
	LocalAddress netip.Addr   `json:"local_address"`
	PeerAddress  netip.Addr   `json:"peer_address"`
	Port         uint16       `json:"port,omitempty"`
	IgnoreIPv4   []netip.Addr `json:"ignore_ipv4,omitempty"`
}

func Validate(c Config) error {
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n:") {
		return errors.New("conntrack sync requires an interface")
	}
	if c.LocalAddress == c.PeerAddress {
		return errors.New("conntrack sync requires distinct IPv4 local and peer addresses")
	}
	for _, addr := range []netip.Addr{c.LocalAddress, c.PeerAddress} {
		if !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() || addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return errors.New("conntrack sync requires IPv4 unicast local and peer addresses")
		}
	}
	for _, addr := range c.IgnoreIPv4 {
		if !addr.Is4() {
			return errors.New("conntrack ignore_ipv4 must contain IPv4 addresses")
		}
	}
	return nil
}

// Render adapts routerd pkg/render.ConntrackdConfig (BSD-3-Clause). Executable,
// socket and log paths belong to the worker's private engine filesystem.
func Render(c Config) []byte {
	port := c.Port
	if port == 0 {
		port = 3780
	}
	var b strings.Builder
	b.WriteString("Sync {\n Mode FTFW {\n ResendQueueSize 131072\n CommitTimeout 180\n PurgeTimeout 60\n ACKWindowSize 300\n DisableExternalCache no\n StartupResync yes\n }\n")
	fmt.Fprintf(&b, " UDP {\n IPv4_address %s\n IPv4_Destination_Address %s\n Port %d\n Interface %s\n SndSocketBuffer 2097152\n RcvSocketBuffer 2097152\n Checksum yes\n }\n", c.LocalAddress, c.PeerAddress, port, c.Interface)
	b.WriteString(" Options {\n TCPWindowTracking no\n ExpectationSync yes\n }\n}\nGeneral {\n HashSize 32768\n HashLimit 262144\n LogFile no\n Syslog no\n LockFile /run/conntrackd.lock\n UNIX {\n Path /run/conntrackd.ctl\n }\n NetlinkBufferSize 2097152\n NetlinkBufferSizeMaxGrowth 8388608\n NetlinkOverrunResync yes\n EventIterationLimit 100\n Filter From Userspace {\n Protocol Accept {\n tcp\n udp\n icmp\n }\n Address Ignore {\n IPv4_address 127.0.0.1\n")
	ignored := append([]netip.Addr{c.LocalAddress, c.PeerAddress}, c.IgnoreIPv4...)
	slices.SortFunc(ignored, netip.Addr.Compare)
	for _, addr := range slices.Compact(ignored) {
		if !addr.IsLoopback() {
			fmt.Fprintf(&b, " IPv4_address %s\n", addr)
		}
	}
	b.WriteString(" IPv6_address ::1\n }\n }\n}\n")
	return []byte(b.String())
}
