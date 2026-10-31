// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

// pruneScopeMismatchedLeases drops an old dynamic binding only when its MAC
// now matches a selected pool and its address cannot be allocated by any of
// that MAC's selected pools. Reservations and unrelated bindings stay intact.
// Run holds the state lock and calls this before dnsmasq opens the file.
func pruneScopeMismatchedLeases(cfg Config) error {
	data, err := os.ReadFile(cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	kept, changed := pruneScopeLeaseData(cfg, string(data))
	if !changed {
		return nil
	}
	return replaceFile(cfg.StatePath, []byte(kept))
}

func pruneScopeLeaseData(cfg Config, data string) (string, bool) {
	if len(cfg.Scopes) == 0 {
		return data, false
	}
	reserved := make(map[string]bool, len(cfg.Reservations))
	for _, reservation := range cfg.Reservations {
		reserved[reservation.MACAddress] = true
	}
	var kept strings.Builder
	changed := false
	for _, record := range strings.SplitAfter(data, "\n") {
		if record == "" {
			continue
		}
		fields := strings.Fields(record)
		if len(fields) != 5 {
			kept.WriteString(record)
			continue
		}
		mac, macErr := net.ParseMAC(fields[1])
		address, addressErr := netip.ParseAddr(fields[2])
		if macErr != nil || addressErr != nil || !address.Is4() || reserved[mac.String()] || !outsideMatchingScopePool(cfg, mac, address) {
			kept.WriteString(record)
			continue
		}
		changed = true
	}
	return kept.String(), changed
}

func outsideMatchingScopePool(cfg Config, mac net.HardwareAddr, address netip.Addr) bool {
	matched := false
	for _, scope := range cfg.Scopes {
		if !scopeMatchesMAC(scope, mac) {
			continue
		}
		matched = true
		start, startErr := netip.ParseAddr(scope.Start)
		end, endErr := netip.ParseAddr(scope.End)
		if startErr == nil && endErr == nil && address.Compare(start) >= 0 && address.Compare(end) <= 0 {
			return false
		}
	}
	return matched
}

func scopeMatchesMAC(scope Scope, mac net.HardwareAddr) bool {
	for _, text := range scope.Match.MACAddresses {
		candidate, err := net.ParseMAC(text)
		if err == nil && bytes.Equal(candidate, mac) {
			return true
		}
	}
	for _, text := range scope.Match.OUIPrefixes {
		oui, err := dhcpserverconfig.ParseOUI(text)
		if err == nil && len(mac) >= len(oui) && bytes.Equal(oui, mac[:len(oui)]) {
			return true
		}
	}
	return false
}
