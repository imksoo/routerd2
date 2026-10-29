//go:build linux

package ntp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"golang.org/x/sys/unix"
)

// runSources owns the learned-source file and its original deadlines. Status
// publication and control requests cannot block expiry. Reloading the source
// directory keeps the clock engine, its measurements and LAN service alive.
func runSources(ctx context.Context, cfg ntpconfig.Config, root *os.File, updates <-chan ntpconfig.Sources) error {
	runtime.LockOSThread() // Keep the creating thread alive through each helper's PDEATHSIG lifetime.
	defer runtime.UnlockOSThread()
	var sources []ntpconfig.LearnedServer
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
			// Unusable optional information does not remove usable sources or
			// configured fallbacks. This is the received-input boundary.
			sources = input.Servers[:0]
			for _, source := range input.Servers {
				if source.ValidUntil.Validate() != nil || source.ValidUntil.BootID == "" || !source.ValidUntil.UTC.IsZero() || ntpconfig.ValidateServer(source.Server, len(cfg.DNS) > 0) != nil {
					continue
				}
				sources = append(sources, source)
			}
		case <-wake:
		}
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		var selected []ntpconfig.Server
		var until uint64
		for _, source := range sources {
			if !source.ValidUntil.Live(clock) {
				continue
			}
			selected = append(selected, source.Server)
			if source.ValidUntil.Nanos != 0 && (until == 0 || source.ValidUntil.Nanos < until) {
				until = source.ValidUntil.Nanos
			}
		}
		// routerd's resolveServers selects learned values while available,
		// then the configured fallback. A changed set gets chronyd's iburst
		// without restarting the clock process or waiting for an idle poll.
		if len(selected) == 0 {
			selected = cfg.Servers
		}
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
			call, cancel := context.WithTimeout(ctx, 2*time.Second)
			command := exec.CommandContext(call, "/proc/self/exe", "chrony-reload-sources")
			command.ExtraFiles = []*os.File{root}
			command.Stdout, command.Stderr = os.Stderr, os.Stderr
			command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
			command.WaitDelay = time.Second
			err = command.Run()
			cancel()
			if err != nil {
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
