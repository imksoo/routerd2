//go:build linux

// Package clocktest is used only by the isolated kernel test image and its
// integration harness. It contains no syscall that can change a clock.
package clocktest

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

const SuiteName = "daemon-dhcp4-clock"
const VRRPSuiteName = "daemon-vrrp-clock"
const DHCP6SuiteName = "daemon-dhcp6-clock"
const RAClientSuiteName = "daemon-ra-clock"

type Guest struct {
	PID                       int
	Suite, Nonce, CommandLine string
}

type Result struct {
	Action string
	Clock  boottime.Clock
}

type Controller struct {
	mu       sync.Mutex
	origin   boottime.Clock
	lastBoot int64
	read     func() (boottime.Clock, error)
	set      func(time.Time) error
}

// NewController must reject ordinary processes, including namespace PID 1,
// before calling either clock function. The per-boot kernel argument must
// match the nonce embedded in the dedicated initramfs.
func NewController(g Guest, read func() (boottime.Clock, error), set func(time.Time) error) (*Controller, error) {
	if g.PID != 1 || g.Suite != SuiteName && g.Suite != VRRPSuiteName && g.Suite != DHCP6SuiteName && g.Suite != RAClientSuiteName || len(g.Nonce) != 32 {
		return nil, errors.New("clock correction requires the dedicated guest PID 1")
	}
	for _, c := range g.Nonce {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errors.New("invalid clock test nonce")
		}
	}
	markers := 0
	for _, arg := range strings.Fields(g.CommandLine) {
		if strings.HasPrefix(arg, "routerd2.clock-test=") {
			markers++
			if arg != "routerd2.clock-test="+g.Nonce {
				return nil, errors.New("clock test kernel marker mismatch")
			}
		}
	}
	if markers != 1 || read == nil || set == nil {
		return nil, errors.New("missing or ambiguous clock test kernel marker")
	}
	clock, err := read()
	if err != nil {
		return nil, err
	}
	if !boottime.ValidBootID(clock.BootID) || clock.BoottimeNanoseconds <= 0 || clock.WallTime.IsZero() {
		return nil, errors.New("invalid initial guest clock")
	}
	return &Controller{origin: clock, lastBoot: clock.BoottimeNanoseconds, read: read, set: set}, nil
}

// Step supports fixed offsets from the original boot baseline, never an
// arbitrary timestamp or a cumulative change based on already corrected UTC.
func (c *Controller) Step(action string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var offset time.Duration
	switch action {
	case "forward":
		offset = 24 * time.Hour
	case "backward":
		offset = -24 * time.Hour
	case "restore":
	default:
		return Result{}, errors.New("unknown fixed clock action")
	}
	clock, err := c.read()
	if err != nil {
		return Result{}, err
	}
	if clock.BootID != c.origin.BootID || clock.BoottimeNanoseconds < c.lastBoot {
		return Result{}, errors.New("guest boot changed or BOOTTIME rewound")
	}
	c.lastBoot = clock.BoottimeNanoseconds
	// Add separately so the duration arithmetic cannot overflow when offset is
	// added to a positive elapsed value.
	target := c.origin.WallTime.Add(time.Duration(clock.BoottimeNanoseconds - c.origin.BoottimeNanoseconds)).Add(offset)
	if err := c.set(target); err != nil {
		return Result{}, err
	}
	clock, err = c.read()
	if err != nil {
		return Result{}, err
	}
	if clock.BootID != c.origin.BootID || clock.BoottimeNanoseconds < c.lastBoot {
		return Result{}, errors.New("guest boot changed after clock correction")
	}
	c.lastBoot = clock.BoottimeNanoseconds
	return Result{Action: action, Clock: clock}, nil
}
