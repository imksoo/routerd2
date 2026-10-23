// SPDX-License-Identifier: BSD-3-Clause

package dhcpserverconfig

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ValidateSnapshot checks the worker's typed boundary. The caller separately
// checks its service envelope, generation/digest, epoch/sequence ordering and
// receipt monotonicity. Replaying an old snapshot must not renew ownership.
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
	if len(s.Leases) > c.MaxLeases || s.Stats.Quarantined < 0 || s.Stats.Quarantined > c.MaxLeases || s.Stats.Offers < 0 || s.Stats.Offers > c.MaxLeases {
		return errors.New("DHCP snapshot exceeds configured bounds")
	}
	if !s.Active {
		if !s.ValidUntil.IsZero() || len(s.Leases) > 0 {
			return errors.New("inactive DHCP snapshot cannot publish leases or a future ownership deadline")
		}
	} else if !s.ValidUntil.After(s.ObservedAt) || s.ValidUntil.After(s.ObservedAt.Add(5*time.Second)) {
		return errors.New("DHCP active ownership must expire within five seconds")
	}
	ids, ips := map[string]bool{}, map[string]bool{}
	for _, l := range s.Leases {
		if err := ValidateLease(c, l, s.ObservedAt); err != nil {
			return fmt.Errorf("DHCP snapshot lease: %w", err)
		}
		key := l.Scope + "|" + l.ClientID
		if l.ClientID == "" {
			key = l.Scope + "|mac:" + l.MACAddress
		}
		if ids[key] || ips[l.IPAddress] {
			return errors.New("DHCP snapshot has duplicate client or address bindings")
		}
		ids[key], ips[l.IPAddress] = true, true
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) > MaxSnapshotBytes {
		return errors.New("DHCP snapshot exceeds 1792 KiB")
	}
	return nil
}

// ValidateLease accepts normalized configuration. Old unexpired journal records
// that no longer fit committed intent remain allocation exclusions, but are not
// published as current DNS ownership until the client obtains a current lease.
func ValidateLease(c Config, l Lease, observed time.Time) error {
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
	a, err := ipv4(l.IPAddress)
	if err != nil {
		return err
	}
	scope := SelectScope(c, mac)
	var reservation *Reservation
	for i := range c.Reservations {
		r := &c.Reservations[i]
		if r.MACAddress == l.MACAddress {
			reservation = r
			if r.Scope != "" {
				if c.Primary.Name == r.Scope {
					scope = c.Primary
				}
				for _, s := range c.Scopes {
					if s.Name == r.Scope {
						scope = s
					}
				}
			}
			break
		}
	}
	if l.Scope != scope.Name {
		return errors.New("lease is outside the client's selected scope")
	}
	prefix, err := netip.ParsePrefix(scope.Prefix)
	if err != nil || !Usable(prefix, a) || l.IPAddress == c.ServerAddress {
		return errors.New("lease is not a usable address in its scope")
	}
	if InfrastructureAddress(c, l.IPAddress) {
		return errors.New("lease assigns the server or a configured router address")
	}
	seconds := scope.LeaseSeconds
	if reservation != nil {
		if !l.Reserved || l.IPAddress != reservation.IPAddress {
			return errors.New("lease does not match its reservation")
		}
		if reservation.Hostname != "" && l.Hostname != reservation.Hostname {
			return errors.New("lease hostname differs from its reservation")
		}
		if reservation.LeaseSeconds != 0 {
			seconds = reservation.LeaseSeconds
		}
	} else {
		start, e1 := netip.ParseAddr(scope.Start)
		end, e2 := netip.ParseAddr(scope.End)
		if l.Reserved || e1 != nil || e2 != nil || a.Compare(start) < 0 || a.Compare(end) > 0 {
			return errors.New("dynamic lease is outside the pool")
		}
		for _, r := range c.Reservations {
			if r.IPAddress == l.IPAddress {
				return errors.New("dynamic lease uses another client's reservation")
			}
		}
	}
	if l.Hostname != "" && !ValidHostname(l.Hostname) {
		return errors.New("invalid lease hostname")
	}
	if l.AcquiredAt.IsZero() || l.RenewedAt.Before(l.AcquiredAt) || l.RenewedAt.After(observed) || !l.ExpiresAt.After(observed) || !l.ExpiresAt.After(l.RenewedAt) || l.ExpiresAt.After(l.RenewedAt.Add(time.Duration(seconds)*time.Second)) {
		return errors.New("lease timestamps exceed committed finite duration")
	}
	return nil
}
