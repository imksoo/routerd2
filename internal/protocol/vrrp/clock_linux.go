//go:build linux

package vrrp

import (
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

const inactiveObservationLifetime = 5 * time.Second

func OwnershipLifetime(c Config) time.Duration {
	c = Normalize(c)
	interval := c.interval()
	return 3*interval + time.Duration(256-c.Priority)*interval/256
}

// stampObservation gives each accepted publication its original absolute boot
// deadline. Renewing an already expired election requires a new transition;
// waking a suspended process cannot silently restore its previous authority.
func (m *machine) stampObservation(clock boottime.Clock) error {
	if !boottime.ValidBootID(clock.BootID) || clock.BoottimeNanoseconds <= 0 || m.bootID != "" && m.bootID != clock.BootID {
		return errors.New("VRRP observation requires the current boot clock")
	}
	now := uint64(clock.BoottimeNanoseconds)
	if now < m.observedNanos || m.state == StateActive && m.observationActive && m.observationTransition == m.transition && now >= m.expiresNanos {
		return errors.New("VRRP observation rewound or active election authority expired")
	}
	span := inactiveObservationLifetime
	if m.state == StateActive {
		span = OwnershipLifetime(m.config)
	}
	if span <= 0 || now > math.MaxInt64-uint64(span) {
		return errors.New("VRRP observation deadline overflow")
	}
	m.bootID, m.observedNanos, m.expiresNanos = clock.BootID, now, now+uint64(span)
	if m.state == StateInitialize {
		m.expiresNanos = 0
	}
	m.observationTransition, m.observationActive = m.transition, m.state == StateActive
	return nil
}

func CurrentObservation(s Snapshot, clock boottime.Clock) bool {
	if !boottime.ValidBootID(s.BootID) || s.BootID != clock.BootID || clock.BoottimeNanoseconds <= 0 || s.ObservedNanos == 0 || s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos > math.MaxInt64 {
		return false
	}
	now := uint64(clock.BoottimeNanoseconds)
	return s.ObservedNanos <= now && now < s.ExpiresNanos
}

func CurrentOwnership(s Snapshot, clock boottime.Clock) bool {
	return s.Active && s.State == StateActive && CurrentObservation(s, clock)
}

// ValidateSnapshot checks the configured identity and bounded wire authority.
// The recipient must separately authenticate the supervisor/producer and current
// boot/clock. UTC display fields never supply or renew ownership.
func ValidateSnapshot(raw Config, s Snapshot) error {
	c := Normalize(raw)
	if err := Validate(c); err != nil {
		return err
	}
	if c.Resource != s.Resource || c.Interface != s.Interface || c.VIPInterface != s.VIPInterface || c.VRF != s.VRF || c.Family != s.Family || c.VirtualRouterID != s.VirtualRouterID || c.Priority != s.Priority || (c.AcceptMode || c.AddressOwner) != s.AcceptMode || c.AddressOwner != s.AddressOwner || s.VirtualMAC != VirtualMAC(c.Family, c.VirtualRouterID).String() || !slices.Equal(c.Addresses, s.Addresses) || c.SourceAddress != "" && s.SourceAddress != c.SourceAddress {
		return errors.New("VRRP snapshot identity or ownership policy mismatch")
	}
	epoch, err := hex.DecodeString(s.Epoch)
	if err != nil || len(epoch) != 16 || len(s.Epoch) != 32 || s.Transition == 0 || !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ObservedNanos > math.MaxInt64 || s.ExpiresNanos > math.MaxInt64 {
		return errors.New("invalid VRRP boot, epoch, transition or observation clock")
	}
	if s.Active != (s.State == StateActive) || s.State != StateActive && s.State != StateBackup && s.State != StateInitialize {
		return errors.New("invalid VRRP state")
	}
	if !s.Active && !s.ValidUntil.IsZero() {
		return errors.New("inactive VRRP state cannot retain an ownership lease")
	}
	if s.State == StateInitialize {
		if s.ExpiresNanos != 0 {
			return errors.New("stopped VRRP cannot retain observation authority")
		}
		return nil
	}
	maximum := inactiveObservationLifetime
	if s.Active {
		maximum = OwnershipLifetime(c)
	}
	if s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos-s.ObservedNanos > uint64(maximum) {
		return errors.New("VRRP observation has invalid or overlong authority")
	}
	return nil
}
