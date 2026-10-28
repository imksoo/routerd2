// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

// ValidateSnapshot checks the worker's typed boundary. The caller separately
// checks its service envelope, generation/digest, epoch/sequence ordering and
// original boot-clock freshness. Replaying a snapshot cannot renew ownership.
func ValidateSnapshot(c Config, s Snapshot) error {
	c = Normalize(c)
	if s.Resource != c.Resource || s.Interface != c.Interface || s.ServerAddress != c.ServerAddress {
		return errors.New("DHCP snapshot identity differs from committed intent")
	}
	epoch, err := hex.DecodeString(s.Epoch)
	if err != nil || len(epoch) != 16 || s.Epoch != strings.ToLower(s.Epoch) || s.Sequence == 0 || s.ObservedAt.IsZero() {
		return errors.New("DHCP snapshot requires a valid epoch, sequence and observation time")
	}
	if len(s.Error) > 2048 {
		return errors.New("DHCP snapshot error text is too large")
	}
	if s.Sync != nil && (c.Replication == nil || c.Standby || len(s.Sync.Error) > 2048) {
		return errors.New("invalid DHCP replication status")
	}
	if len(s.Leases) > c.MaxLeases {
		return errors.New("DHCP snapshot exceeds configured bounds")
	}
	if !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos-s.ObservedNanos > uint64(ObservationLifetime) {
		return errors.New("DHCP observation requires a boot-scoped deadline of at most five seconds")
	}
	if s.Standby != c.Standby || c.Standby && s.Active {
		return errors.New("DHCP standby mode differs from committed intent")
	}
	if !s.Active && len(s.Leases) != 0 {
		return errors.New("inactive DHCP snapshot cannot publish leases")
	}
	ips := map[string]bool{}
	for _, l := range s.Leases {
		if err := validateLease(l, s.ObservedAt); err != nil {
			return fmt.Errorf("DHCP snapshot lease: %w", err)
		}
		if ips[l.IPAddress] {
			return errors.New("DHCP snapshot has duplicate address bindings")
		}
		ips[l.IPAddress] = true
	}
	return nil
}

// Only externally consumed lease facts cross this boundary. Pool selection
// and reservation policy belong to dnsmasq, not a second Go allocator.
func validateLease(l Lease, observed time.Time) error {
	mac, err := net.ParseMAC(l.MACAddress)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() != l.MACAddress {
		return errors.New("invalid Ethernet client identity")
	}
	id, err := hex.DecodeString(l.ClientID)
	if err != nil || len(id) > 255 || l.ClientID != "" && len(id) < 2 || strings.ToLower(l.ClientID) != l.ClientID {
		return errors.New("invalid client identifier")
	}
	if len(id) == 0 && l.MACAddress == "00:00:00:00:00:00" {
		return errors.New("missing client identity")
	}
	if _, err := ipv4(l.IPAddress); err != nil {
		return err
	}
	if l.Hostname != "" && !ValidHostname(l.Hostname) {
		return errors.New("invalid lease hostname")
	}
	if !l.ExpiresAt.After(observed) {
		return errors.New("lease has expired")
	}
	return nil
}
