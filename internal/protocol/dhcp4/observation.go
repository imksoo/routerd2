package dhcp4

import (
	"errors"
	"math"
	"net"
	"net/netip"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/boottime"
)

// LeaseClock is fixed by a server ACK, never by receipt, retry or display.
// Inactive INIT-REBOOT hints can retain a former boot's metadata but no authority.
type LeaseClock struct {
	BootID        string `json:"boot_id"`
	Sequence      uint64 `json:"sequence"`
	Epoch         string `json:"epoch"`
	AcquiredNanos uint64 `json:"acquired_nanos"`
	RenewNanos    uint64 `json:"renew_nanos"`
	RebindNanos   uint64 `json:"rebind_nanos"`
	ExpiresNanos  uint64 `json:"expires_nanos"`
}

func validateLeaseClock(s Snapshot) error {
	l := s.LeaseClock
	if !boottime.ValidBootID(l.BootID) || !boottime.ValidBootID(l.Epoch) || l.Sequence == 0 || l.AcquiredNanos == 0 || l.AcquiredNanos >= l.RenewNanos || l.RenewNanos >= l.RebindNanos || l.RebindNanos >= l.ExpiresNanos || l.ExpiresNanos > math.MaxInt64 || s.LeaseTimeSeconds <= 0 || s.LeaseTimeSeconds > math.MaxUint32 || s.AcquiredAt.IsZero() {
		return errors.New("invalid DHCPv4 original boot lease interval")
	}
	life := time.Duration(s.LeaseTimeSeconds) * time.Second
	if l.ExpiresNanos-l.AcquiredNanos != uint64(life) || !s.ExpiresAt.Equal(s.AcquiredAt.Add(life)) || s.RenewAt.Sub(s.AcquiredAt) != time.Duration(l.RenewNanos-l.AcquiredNanos) || s.RebindAt.Sub(s.AcquiredAt) != time.Duration(l.RebindNanos-l.AcquiredNanos) {
		return errors.New("DHCPv4 UTC display differs from original boot lease intervals")
	}
	return nil
}

func ValidateObservation(s Snapshot) error {
	if !boottime.ValidBootID(s.BootID) || !boottime.ValidBootID(s.Epoch) || s.Sequence == 0 || s.ObservedNanos == 0 || s.ObservedNanos > math.MaxInt64-uint64(5*time.Second) || s.UpdatedAt.IsZero() || s.UpdatedAt.Year() < 1970 || s.UpdatedAt.Year() > 9999 || s.Stopped && s.Active {
		return errors.New("invalid DHCPv4 boot observation")
	}
	if s.LeaseClock != (LeaseClock{}) {
		if err := validateLeaseClock(s); err != nil {
			return err
		}
	}
	if !s.Active {
		return nil
	}
	if s.LeaseClock == (LeaseClock{}) || s.LeaseClock.BootID != s.BootID || s.LeaseClock.Epoch != s.Epoch || s.LeaseClock.Sequence > s.Sequence || s.LeaseClock.AcquiredNanos > s.ObservedNanos || s.ObservedNanos >= s.LeaseClock.ExpiresNanos {
		return errors.New("active DHCPv4 omitted its original current-boot grant")
	}
	switch s.State {
	case wire.StateBound, wire.StateRenewing, wire.StateRebinding:
	default:
		return errors.New("DHCPv4 active in a nonlease state")
	}
	address, aerr := netip.ParseAddr(s.CurrentAddress)
	server, serr := netip.ParseAddr(s.ServerID)
	mac, merr := net.ParseMAC(s.HardwareAddress)
	if aerr != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() || serr != nil || !server.Is4() || server.IsUnspecified() || server.IsMulticast() || s.PrefixLength < 0 || s.PrefixLength > 32 || merr != nil || len(mac) != 6 {
		return errors.New("invalid active DHCPv4 lease identity")
	}
	return nil
}

func ValidateSnapshot(c Config, s Snapshot) error {
	if s.Resource != c.Resource || s.Interface != c.Interface || s.ClientIdentity != c.ClientID {
		return errors.New("DHCPv4 snapshot identity mismatch")
	}
	return ValidateObservation(s)
}

func CurrentLease(s Snapshot, clock boottime.Clock) bool {
	return s.Active && ValidateObservation(s) == nil && s.BootID == clock.BootID && clock.BoottimeNanoseconds > 0 && s.ObservedNanos <= uint64(clock.BoottimeNanoseconds) && uint64(clock.BoottimeNanoseconds) < s.LeaseClock.ExpiresNanos
}
