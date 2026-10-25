//go:build linux

package dhcp4

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"golang.org/x/sys/unix"
)

// Run calls ready after socket and durable-state initialization. Readiness does
// not assert that a server has granted a lease. A callback error is fatal.
func Run(ctx context.Context, cfg Config, effects Effects, ready func()) (result error) {
	if err := Validate(cfg); err != nil {
		return err
	}
	if effects == nil {
		return errors.New("dhcp4 effects are required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("DHCPv4 state is already owned: %w", err)
	}
	ifi, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return err
	}
	if len(ifi.HardwareAddr) != 6 {
		return errors.New("DHCPv4 requires an Ethernet interface")
	}
	conn, err := listenDHCPv4(cfg.Interface)
	if err != nil {
		return err
	}
	defer conn.Close()
	var prior *Snapshot
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		var s Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode DHCPv4 state: %w", err)
		}
		if s.Resource != cfg.Resource || s.Interface != cfg.Interface {
			return errors.New("DHCPv4 journal belongs to another resource/interface")
		}
		prior = &s
	}
	c := &client{config: cfg, hardware: ifi.HardwareAddr, conn: conn, effects: effects, readClock: bfd.ReadClock, xid: wire.NewXID, persist: func(s Snapshot) error { return writeSnapshot(cfg.StatePath, s) }}
	if err := c.start(ctx, prior); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	defer func() {
		withdraw := c.snapshot()
		withdraw.Active = false
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := effects.Update(cleanup, withdraw); err != nil {
			result = errors.Join(result, fmt.Errorf("withdraw DHCPv4 effects: %w", err))
		}
	}()
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.tick(ctx); err != nil {
			return err
		}
		deadline := time.Now().Add(250 * time.Millisecond)
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if from == nil || from.Port != 67 {
			continue
		}
		if err := c.handle(ctx, buf[:n]); err != nil {
			return err
		}
	}
}
func writeSnapshot(path string, s Snapshot) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp4-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
