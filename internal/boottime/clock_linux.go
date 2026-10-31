//go:build linux

// Package boottime reads a boot-scoped Linux elapsed clock. CLOCK_BOOTTIME
// includes suspend time. DHCPv6 uses WallTime only to evaluate a saved PD
// expiry after a new boot has invalidated that elapsed clock.
package boottime

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type Clock struct {
	BootID              string
	BoottimeNanoseconds int64
	WallTime            time.Time
}

type processClockReader struct {
	identity atomic.Pointer[string]
	mu       sync.Mutex
	load     func() (string, error)
}

var processClock = processClockReader{load: loadKernelBootID}

func Read() (Clock, error) { return processClock.Read() }

func (r *processClockReader) Read() (Clock, error) {
	id := r.identity.Load()
	if id == nil {
		r.mu.Lock()
		id = r.identity.Load()
		if id == nil {
			value, err := r.load()
			if err == nil && !ValidBootID(value) {
				err = errors.New("invalid kernel boot ID")
			}
			if err != nil {
				r.mu.Unlock()
				return Clock{}, err
			}
			r.identity.Store(&value)
			id = &value
		}
		r.mu.Unlock()
	}
	return ReadForBoot(*id)
}

func ValidBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, ch := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
			continue
		}
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func loadKernelBootID() (string, error) {
	f, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(value))
	if len(value) > 64 || !ValidBootID(id) {
		return "", errors.New("invalid kernel boot ID")
	}
	return id, nil
}

// ReadForBoot is used with an already validated immutable process boot ID.
func ReadForBoot(id string) (Clock, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return Clock{}, err
	}
	value := ts.Nano()
	if value <= 0 {
		return Clock{}, errors.New("invalid CLOCK_BOOTTIME value")
	}
	return Clock{BootID: id, BoottimeNanoseconds: value, WallTime: time.Now().UTC()}, nil
}
