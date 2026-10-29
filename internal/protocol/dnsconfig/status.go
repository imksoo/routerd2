package dnsconfig

import (
	"errors"
	"net/netip"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

const ObservationLifetime = 2 * time.Second

// Snapshot reports sockets actually owned by the DNS worker. DHCP serving can
// depend on them without guessing that input delivery already opened a socket.
type Snapshot struct {
	Resource      string           `json:"resource"`
	BootID        string           `json:"boot_id"`
	ObservedNanos uint64           `json:"observed_nanos"`
	ExpiresNanos  uint64           `json:"expires_nanos"`
	Listeners     []netip.AddrPort `json:"listeners"`
}

func ValidateSnapshot(s Snapshot) error {
	if s.Resource == "" || !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos-s.ObservedNanos > uint64(ObservationLifetime) || len(s.Listeners) > 1024 {
		return errors.New("invalid DNS listener observation")
	}
	for _, address := range s.Listeners {
		if !address.IsValid() || address.Port() == 0 || address.Addr().Is4In6() || address.Addr().IsMulticast() {
			return errors.New("invalid DNS listening endpoint")
		}
	}
	return nil
}

func (s Snapshot) Current(clock boottime.Clock) bool {
	return s.BootID == clock.BootID && s.ObservedNanos <= uint64(clock.BoottimeNanoseconds) && uint64(clock.BoottimeNanoseconds) < s.ExpiresNanos
}
