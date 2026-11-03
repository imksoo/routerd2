//go:build linux

package dhcp6server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/linklocal"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsmasqengine"
	"golang.org/x/sys/unix"
)

// dnsmasq owns DHCPv6 exchanges and leases, as in routerd's LAN renderer.
// RA remains owned by the configured RA service; this engine never sends RA.
func statefulConfig(c dhcp6serverconfig.Config, in dhcp6serverconfig.Input, clock boottime.Clock) ([]byte, time.Duration) {
	lines := []string{"port=0", "no-resolv", "no-hosts", "bind-dynamic", "user=root", "group=root", "log-facility=-", "dhcp-lease-max=" + strconv.Itoa(dhcp6serverconfig.MaxLeases), "interface=" + c.Interface, "pid-file=/state/" + filepath.Base(c.StatePath) + ".pid", "dhcp-leasefile=/state/" + filepath.Base(c.StatePath) + ".leases"}
	if c.LogDHCP {
		lines = append(lines, "log-dhcp")
	}
	lease := c.Pool.LeaseSeconds
	if lease == 0 {
		lease = 43200
	}
	prefix := c.Pool.PrefixLength
	if prefix == 0 {
		prefix = 64
	}
	addresses := c.Pool.Start.String() + "," + c.Pool.End.String()
	if netip.PrefixFrom(c.Pool.Start, int(prefix)).Masked().Addr().IsUnspecified() {
		addresses += ",constructor:" + c.Interface
	}
	lines = append(lines, fmt.Sprintf("dhcp-range=%s,%d,%ds", addresses, prefix, lease), fmt.Sprintf("dhcp-option=option6:information-refresh-time,%d", c.RefreshSeconds))
	next := time.Hour
	live := func(deadlineTime time.Duration) {
		if deadlineTime < next {
			next = max(0, deadlineTime)
		}
	}
	for _, grant := range []lifetime.Deadline{in.Authority, in.DNSReadyUntil} {
		if grant.Nanos != 0 {
			live(time.Duration(grant.Nanos) - time.Duration(clock.BoottimeNanoseconds))
		}
	}
	add := func(code string, static []netip.Addr, learned []dhcp6serverconfig.Address) {
		values := make([]string, 0, len(static)+len(learned))
		seen := map[netip.Addr]bool{}
		for _, ip := range static {
			seen[ip] = true
			values = append(values, "["+ip.String()+"]")
		}
		for _, entry := range learned {
			if !entry.ValidUntil.Live(clock) {
				continue
			}
			if entry.ValidUntil.Nanos != 0 {
				live(time.Duration(entry.ValidUntil.Nanos) - time.Duration(clock.BoottimeNanoseconds))
			}
			if !seen[entry.Address] {
				seen[entry.Address] = true
				values = append(values, "["+entry.Address.String()+"]")
			}
		}
		if len(values) > 0 {
			lines = append(lines, "dhcp-option=option6:"+code+","+strings.Join(values, ","))
		}
	}
	add("dns-server", c.DNSServers, in.DNSServers)
	add("sntp-server", c.SNTPServers, in.SNTPServers)
	domains := append([]string(nil), c.DomainSearch...)
	for _, entry := range in.DomainSearch {
		if entry.ValidUntil.Live(clock) {
			domains = append(domains, entry.Name)
			if entry.ValidUntil.Nanos != 0 {
				live(time.Duration(entry.ValidUntil.Nanos) - time.Duration(clock.BoottimeNanoseconds))
			}
		}
	}
	if len(domains) > 0 {
		lines = append(lines, "dhcp-option=option6:domain-search,"+strings.Join(domains, ","))
	}
	return []byte(strings.Join(lines, "\n") + "\n"), next
}

func seedLeaseIdentity(path string, server []byte) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parts := make([]string, len(server))
	for i, b := range server {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	header := "duid " + strings.Join(parts, ":")
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "duid ") {
			lines = append(lines, line)
		}
	}
	updated := []byte(header + "\n" + strings.Join(lines, "\n"))
	if len(lines) > 0 {
		updated = append(updated, '\n')
	}
	if bytes.Equal(data, updated) {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dhcp6-leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(updated); err != nil {
		file.Close()
		return err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func runStateful(ctx context.Context, c dhcp6serverconfig.Config, server []byte, in dhcp6serverconfig.Input, updates <-chan dhcp6serverconfig.Input, report func(context.Context, dhcp6serverconfig.Snapshot) error, ready func()) (result error) {
	if _, _, err := linklocal.Wait(ctx, c.Interface, netip.Addr{}); err != nil {
		return err
	}
	image := os.NewFile(5, "dnsmasq-engine")
	defer image.Close()
	if _, err := image.Stat(); err != nil {
		return fmt.Errorf("stateful DHCPv6 engine: %w", err)
	}
	if err := seedLeaseIdentity(c.StatePath+".leases", server); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var child *exec.Cmd
	var done chan error
	stop := func() error {
		if child == nil {
			return nil
		}
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			child = nil
			done = nil
			return nil
		case <-time.After(10 * time.Second):
			_ = child.Process.Kill()
			<-done
			child = nil
			done = nil
			return errors.New("DHCPv6 engine did not stop")
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	var current []byte
	var grants atomic.Pointer[leaseGrant]
	var observerDone chan struct{}
	observeCtx, stopObserver := context.WithCancel(ctx)
	defer func() {
		stopObserver()
		if err := stop(); err != nil {
			result = errors.Join(result, err)
		}
		if observerDone != nil {
			<-observerDone
		}
	}()
	for {
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		configured, next := statefulConfig(c, in, clock)
		if !in.Authority.Live(clock) || !in.DNSReadyUntil.Live(clock) {
			configured = nil
			next = time.Hour
		}
		timer.Reset(next)
		if !bytes.Equal(configured, current) {
			if err := stop(); err != nil {
				return err
			}
			current = nil
			if len(configured) > 0 {
				fd, err := unix.MemfdCreate("dnsmasq6-config", unix.MFD_CLOEXEC)
				if err != nil {
					return err
				}
				settings := os.NewFile(uintptr(fd), "dnsmasq6-config")
				err = json.NewEncoder(settings).Encode(dnsmasqengine.Settings{Config: configured, StateDirectory: filepath.Dir(c.StatePath)})
				if err == nil {
					_, err = settings.Seek(0, io.SeekStart)
				}
				if err != nil {
					settings.Close()
					return err
				}
				child = exec.Command("/proc/self/exe", "dnsmasq-engine")
				child.ExtraFiles = []*os.File{settings, image}
				child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
				log := slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo).Writer()
				child.Stdout, child.Stderr = log, log
				err = child.Start()
				settings.Close()
				if err != nil {
					child = nil
					return err
				}
				done = make(chan error, 1)
				go func(command *exec.Cmd, completion chan<- error) { completion <- command.Wait() }(child, done)
				current = configured
				// dnsmasq writes the PID only after its sockets and lease file are ready.
				startup := time.NewTicker(50 * time.Millisecond)
				started := false
				for !started {
					select {
					case <-ctx.Done():
						startup.Stop()
						return nil
					case err := <-done:
						startup.Stop()
						child = nil
						done = nil
						return fmt.Errorf("DHCPv6 engine startup: %v", err)
					case <-startup.C:
						data, err := os.ReadFile(c.StatePath + ".pid")
						started = err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(child.Process.Pid)
					}
				}
				startup.Stop()
			}
		}
		grants.Store(&leaseGrant{active: child != nil, until: in.Authority})
		if observerDone == nil {
			observerDone = make(chan struct{})
			go func() { defer close(observerDone); observeLeases(observeCtx, c, server, &grants, report, ready) }()
		}

		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			child = nil
			done = nil
			return fmt.Errorf("DHCPv6 engine exited: %v", err)
		case next, open := <-updates:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("DHCPv6 server input owner stopped")
			}
			if err := dhcp6serverconfig.ValidateInput(next); err != nil {
				return err
			}
			in = next
		case <-timer.C:
		}
	}
}
