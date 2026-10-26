//go:build linux

package network

import (
	"errors"
	"fmt"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"golang.org/x/sys/unix"
)

// Linux's IPv6 RTA_EXPIRES is a relative jiffies timer. Its persisted physical
// deadline uses CLOCK_MONOTONIC; the protocol authority retains its own clock.
// UTC changes cannot turn a managed kernel timer into apparently foreign drift.
type routeTimer struct {
	BootID string `json:"boot_id,omitempty"`
	Nanos  uint64 `json:"nanos,omitempty"`
}
type ownedRoute struct {
	Route           `json:"route"`
	KernelExpiry    routeTimer  `json:"kernel_expiry,omitzero"`
	AttemptedExpiry *routeTimer `json:"attempted_expiry,omitempty"`
}
type routeClock struct {
	boottime.Clock
	monotonicNanos int64
}

const routeKernelGrant = 7 * 24 * time.Hour
const routeKernelRenewBefore = 24 * time.Hour

func readRouteClock() (routeClock, error) {
	c, err := boottime.Read()
	if err != nil {
		return routeClock{}, err
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return routeClock{}, err
	}
	return routeClock{Clock: c, monotonicNanos: ts.Nano()}, nil
}
func newRouteTimer(d lifetime.Deadline, c routeClock) (routeTimer, error) {
	if d == (lifetime.Deadline{}) || d.BootID != "" && d.Nanos == 0 && d.BootID == c.BootID {
		return routeTimer{}, nil
	}
	remaining := d.Remaining(c.Clock)
	if remaining <= 0 {
		return routeTimer{}, errors.New("route lifetime expired before kernel installation")
	}
	seconds := (min(remaining, routeKernelGrant) + time.Second - 1) / time.Second
	return routeTimer{BootID: c.BootID, Nanos: uint64(c.monotonicNanos) + uint64(seconds*time.Second)}, nil
}
func sameRouteTimer(a, b routeTimer) bool {
	if a.BootID != b.BootID {
		return false
	}
	if a.Nanos == 0 || b.Nanos == 0 {
		return a.Nanos == b.Nanos
	}
	delta := int64(a.Nanos) - int64(b.Nanos)
	// RTA_EXPIRES rounds to seconds; dumps round to USER_HZ. Include the
	// small interval between the persisted intent and its netlink send.
	return delta >= -int64(2*time.Second) && delta <= int64(2*time.Second)
}
func validateRouteJournal(routes []ownedRoute) error {
	definitions := make([]Route, 0, len(routes))
	for _, r := range routes {
		timer := r.KernelExpiry
		if r.AttemptedExpiry != nil {
			timer = *r.AttemptedExpiry
		}
		finite := !r.ValidUntil.UTC.IsZero() || r.ValidUntil.BootID != "" && r.ValidUntil.Nanos != 0
		if finite != (timer != (routeTimer{})) {
			return fmt.Errorf("route %s kernel timer differs from lifetime", r.Destination)
		}
		if r.ValidUntil.BootID != "" && timer.BootID != "" && r.ValidUntil.BootID != timer.BootID {
			return fmt.Errorf("route %s timer belongs to another boot", r.Destination)
		}
		for _, observed := range []routeTimer{r.KernelExpiry, timer} {
			if observed != (routeTimer{}) && (!boottime.ValidBootID(observed.BootID) || observed.Nanos == 0 || observed.Nanos > 1<<63-1) {
				return fmt.Errorf("invalid kernel timer for route %s", r.Destination)
			}
		}
		definitions = append(definitions, r.Route)
	}
	return Validate(Desired{Routes: definitions})
}
