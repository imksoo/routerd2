//go:build linux

package ntp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"golang.org/x/sys/unix"
)

// runSources owns the learned-source file and its original deadlines. Status
// publication and control requests cannot block expiry. Reloading the source
// directory keeps the clock engine, its measurements and LAN service alive.
func runSources(ctx context.Context, cfg ntpconfig.Config, root *os.File, connection *net.UnixConn, updates <-chan ntpconfig.Sources) error {
	grants := make([]ntpconfig.SourceGrant, len(cfg.Sources))
	var applied []byte
	timer := time.NewTimer(0)
	defer timer.Stop()
	var wake <-chan time.Time = timer.C
	for {
		select {
		case <-ctx.Done():
			return nil
		case input, open := <-updates:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("NTP source producer stopped")
			}
			if len(input.Groups) != len(cfg.Sources) {
				return errors.New("NTP source grant count does not match configuration")
			}
			grants = input.Groups
			for i := range grants {
				group := &grants[i]
				if group.ActiveUntil != nil && (group.ActiveUntil.Validate() != nil || group.ActiveUntil.BootID == "" || !group.ActiveUntil.UTC.IsZero()) {
					return errors.New("invalid NTP source activation deadline")
				}
				// Keep usable independently learned information.
				valid := group.Servers[:0]
				for _, source := range group.Servers {
					if source.ValidUntil.Validate() != nil || source.ValidUntil.BootID == "" || !source.ValidUntil.UTC.IsZero() || ntpconfig.ValidateServer(source.Server, len(cfg.DNS) > 0) != nil {
						continue
					}
					valid = append(valid, source)
				}
				group.Servers = valid
			}
		case <-wake:
		}
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		var selected []ntpconfig.Server
		var until uint64
		for i, source := range cfg.Sources {
			if source.When == nil && len(source.SourcesFrom) == 0 {
				continue
			} // Rendered in the fixed engine configuration.
			grant := grants[i]
			if source.When != nil {
				if grant.ActiveUntil == nil || !grant.ActiveUntil.Live(clock) {
					continue
				}
				if deadline := grant.ActiveUntil.Nanos; deadline != 0 && (until == 0 || deadline < until) {
					until = deadline
				}
			}
			before := len(selected)
			for _, learned := range grant.Servers {
				if !learned.ValidUntil.Live(clock) {
					continue
				}
				selected = append(selected, learned.Server)
				if deadline := learned.ValidUntil.Nanos; deadline != 0 && (until == 0 || deadline < until) {
					until = deadline
				}
			}
			if len(selected) == before {
				selected = append(selected, source.Servers...)
			}
		}
		// Different client/server fragments can intentionally name the same
		// source; chronyd must receive each endpoint once.
		unique := selected[:0]
		seen := make(map[ntpconfig.Server]bool, len(selected))
		for _, source := range cfg.Sources {
			if source.When == nil && len(source.SourcesFrom) == 0 {
				for _, server := range source.Servers {
					seen[server] = true
				}
			}
		}
		for _, server := range selected {
			if !seen[server] {
				seen[server] = true
				unique = append(unique, server)
			}
		}
		selected = unique
		body := ntpconfig.RenderServers(selected)
		if !bytes.Equal(body, applied) {
			fd, err := unix.Openat(int(root.Fd()), "run/sources/learned.sources", unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC, 0600)
			if err != nil {
				return fmt.Errorf("open learned NTP sources: %w", err)
			}
			file := os.NewFile(uintptr(fd), "learned NTP sources")
			_, writeErr := file.Write(body)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				return fmt.Errorf("write learned NTP sources: %w", err)
			}
			if err := reloadSources(connection); err != nil {
				return fmt.Errorf("reload learned NTP sources: %w", err)
			}
			applied = body
		}
		timer.Stop()
		wake = nil
		if until != 0 {
			clock, err = boottime.Read()
			if err != nil {
				return err
			}
			// Go timers exclude suspend. Re-read BOOTTIME after resuming.
			timer.Reset(max(0, min(time.Duration(until)-time.Duration(clock.BoottimeNanoseconds), time.Second)))
			wake = timer.C
		}
	}
}
