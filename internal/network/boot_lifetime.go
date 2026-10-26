package network

import (
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

// InfiniteNanos denotes an unbounded budget within its originating boot.
const InfiniteNanos = uint64(math.MaxUint64)

// BootLifetime is controller-generated address lifetime intent. It is not an
// operator configuration field. Deadlines survive writer restart in this boot.
type BootLifetime struct {
	BootID         string `json:"boot_id"`
	PreferredNanos uint64 `json:"preferred_nanos"`
	ValidNanos     uint64 `json:"valid_nanos"`
}

func (b BootLifetime) valid() bool {
	return boottime.ValidBootID(b.BootID) && b.ValidNanos != 0 && (b.ValidNanos <= math.MaxInt64 || b.ValidNanos == InfiniteNanos) && (b.PreferredNanos <= math.MaxInt64 || b.PreferredNanos == InfiniteNanos) && b.PreferredNanos <= b.ValidNanos
}
func addressLifetimesAt(o AddressOptions, now time.Time, clock boottime.Clock) (uint32, uint32, error) {
	b := o.BootLifetime
	if b == (BootLifetime{}) {
		return lifetimeRemaining(o.PreferredUntil, now), lifetimeRemaining(o.ValidUntil, now), nil
	}
	if !b.valid() || b.BootID != clock.BootID || clock.BoottimeNanoseconds <= 0 || !o.PreferredUntil.IsZero() || !o.ValidUntil.IsZero() {
		return 0, 0, errors.New("invalid or former-boot address lifetime")
	}
	remaining := func(until uint64) uint32 {
		if until == InfiniteNanos {
			return math.MaxUint32
		}
		now := uint64(clock.BoottimeNanoseconds)
		if until <= now {
			return 0
		}
		seconds := (until - now + uint64(time.Second) - 1) / uint64(time.Second)
		if seconds >= math.MaxUint32 {
			return math.MaxUint32 - 1
		}
		return uint32(seconds)
	}
	return remaining(b.PreferredNanos), remaining(b.ValidNanos), nil
}

func addressLifetimes(o AddressOptions, now time.Time) (uint32, uint32, error) {
	var clock boottime.Clock
	if o.BootLifetime != (BootLifetime{}) {
		var err error
		clock, err = boottime.Read()
		if err != nil {
			return 0, 0, err
		}
	}
	return addressLifetimesAt(o, now, clock)
}

func addressOptionsMatchAt(live AddressState, want AddressOptions, now time.Time, clock boottime.Clock) bool {
	p, v, err := addressLifetimesAt(want, now, clock)
	return err == nil && live.Peer == want.Peer && (live.Flags&addressNoDADFlag != 0) == want.NoDAD && lifetimeClose(live.PreferredLifetime, p) && lifetimeClose(live.ValidLifetime, v)
}
