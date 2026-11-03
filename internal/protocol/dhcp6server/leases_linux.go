//go:build linux

package dhcp6server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
)

// The engine owns allocations and the file. As in routerd's zone reader,
// independently usable rows survive unrelated lines and optional information.
func readLeases(path string, now time.Time) ([]dhcp6serverconfig.Lease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var leases []dhcp6serverconfig.Lease
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		seconds, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		expiry := time.Unix(seconds, 0).UTC()
		if !expiry.After(now) {
			continue
		}
		address, err := netip.ParseAddr(fields[2])
		if err != nil || !address.Is6() || address.Is4In6() || address.Zone() != "" || !address.IsGlobalUnicast() {
			continue
		}
		hostname := fields[3]
		if hostname == "*" {
			hostname = ""
		}
		leases = append(leases, dhcp6serverconfig.Lease{Address: address, Hostname: hostname, ExpiresAt: expiry})
		if len(leases) > dhcp6serverconfig.MaxLeases {
			return nil, errors.New("DHCPv6 lease file exceeds the engine's lease bound")
		}
	}
	return leases, nil
}

// The process owner publishes this immutable serving grant after each input or
// engine transition. File/HTTP work cannot block that owner's expiry or stop.
type leaseGrant struct {
	active bool
	until  lifetime.Deadline
}

func observeLeases(ctx context.Context, c dhcp6serverconfig.Config, server []byte, grant *atomic.Pointer[leaseGrant], report func(context.Context, dhcp6serverconfig.Snapshot) error, ready func()) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		err := func() error {
			clock, err := boottime.Read()
			if err != nil {
				return err
			}
			source := grant.Load()
			s := dhcp6serverconfig.Snapshot{Resource: c.Resource, Interface: c.Interface, ServerDUID: hex.EncodeToString(server), Active: source.active && source.until.Live(clock), ObservedAt: clock.WallTime, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(dhcp6serverconfig.ObservationLifetime)}
			if s.Active {
				if source.until.Nanos != 0 {
					s.ExpiresNanos = min(s.ExpiresNanos, source.until.Nanos)
				}
				s.Leases, err = readLeases(c.StatePath+".leases", clock.WallTime)
				if err != nil {
					return fmt.Errorf("observe DHCPv6 leases: %w", err)
				}
			}
			call, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := report(call, s); err != nil {
				return err
			}
			if ready != nil {
				ready()
				ready = nil
			}
			return nil
		}()
		if err != nil && ctx.Err() == nil {
			slog.Warn("DHCPv6 lease publication failed", "resource", c.Resource, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
