// Package lifetime preserves the clock that owns an expiry: an operator's UTC
// cutoff or a learned protocol budget in the originating Linux boot.
package lifetime

import (
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

// A zero Deadline is unbounded. With BootID set, Nanos zero means an unbounded
// lifetime in that boot only. UTC and BootID/Nanos are mutually exclusive.
type Deadline struct {
	UTC    time.Time `json:"utc,omitzero"`
	BootID string    `json:"boot_id,omitempty"`
	Nanos  uint64    `json:"nanos,omitempty"`
}

func (d Deadline) Validate() error {
	if !d.UTC.IsZero() {
		if d.BootID != "" || d.Nanos != 0 || d.UTC.Unix() < 0 || d.UTC.UTC().Year() > 9999 {
			return errors.New("invalid UTC deadline")
		}
		return nil
	}
	if d.BootID != "" {
		if !boottime.ValidBootID(d.BootID) || d.Nanos > math.MaxInt64 {
			return errors.New("invalid boot deadline")
		}
	} else if d.Nanos != 0 {
		return errors.New("boot deadline requires boot_id")
	}
	return nil
}
func (d Deadline) Remaining(clock boottime.Clock) time.Duration {
	if !d.UTC.IsZero() {
		if clock.WallTime.IsZero() {
			return 0
		}
		return max(0, d.UTC.Sub(clock.WallTime))
	}
	if d.BootID != "" {
		if d.BootID != clock.BootID || clock.BoottimeNanoseconds <= 0 {
			return 0
		}
		if d.Nanos != 0 {
			now := uint64(clock.BoottimeNanoseconds)
			if d.Nanos <= now {
				return 0
			}
			return time.Duration(d.Nanos - now)
		}
	}
	return time.Duration(math.MaxInt64)
}
func (d Deadline) Live(clock boottime.Clock) bool { return d.Remaining(clock) > 0 }
func (d Deadline) Seconds(maximum uint32, clock boottime.Clock) uint32 {
	return uint32(min(uint64(maximum), uint64(d.Remaining(clock)/time.Second)))
}

// Minimum returns an original deadline, never a newly rebased expiry.
func Minimum(clock boottime.Clock, values ...Deadline) Deadline {
	var out Deadline
	remaining := time.Duration(math.MaxInt64)
	for _, d := range values {
		left := d.Remaining(clock)
		if left < remaining || out == (Deadline{}) && d != (Deadline{}) {
			out, remaining = d, left
		}
	}
	return out
}

// After starts a protocol budget at the packet observation, including suspend.
// DHCP/RA's all-ones seconds value is explicit infinity in the same boot.
func After(clock boottime.Clock, seconds uint32) Deadline {
	d := Deadline{BootID: clock.BootID}
	if seconds != math.MaxUint32 {
		d.Nanos = uint64(clock.BoottimeNanoseconds) + uint64(seconds)*uint64(time.Second)
	}
	return d
}

// Ordered validates a preferred/valid pair owned by the same clock.
func Ordered(preferred, valid Deadline) bool {
	if valid == (Deadline{}) {
		return true
	}
	if preferred == (Deadline{}) {
		return false
	}
	if !preferred.UTC.IsZero() || !valid.UTC.IsZero() {
		return !preferred.UTC.IsZero() && !valid.UTC.IsZero() && !preferred.UTC.After(valid.UTC)
	}
	return preferred.BootID == valid.BootID && (valid.Nanos == 0 || preferred.Nanos != 0 && preferred.Nanos <= valid.Nanos)
}
