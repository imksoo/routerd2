//go:build linux

// Package dhcp6server serves DHCPv6 information on a configured LAN interface.
package dhcp6server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/linklocal"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

func Run(ctx context.Context, cfg dhcp6serverconfig.Config, initial dhcp6serverconfig.Input, updates <-chan dhcp6serverconfig.Input, report func(context.Context, dhcp6serverconfig.Snapshot) error, ready func()) error {
	if err := dhcp6serverconfig.Validate(cfg); err != nil {
		return err
	}
	if err := dhcp6serverconfig.ValidateInput(initial); err != nil {
		return err
	}
	if cfg.RefreshSeconds == 0 {
		cfg.RefreshSeconds = 86400
	}
	identity, err := loadIdentity(ctx, cfg)
	if err != nil {
		return err
	}
	defer identity.lock.Close()
	server := identity.duid
	iface, source, err := linklocal.Wait(ctx, cfg.Interface, netip.Addr{})
	if err != nil {
		return err
	}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, cfg.Interface)
		})
		return errors.Join(err, inner)
	}}
	conn, err := lc.ListenPacket(ctx, "udp6", "[::]:547")
	if err != nil {
		return err
	}
	packet := ipv6.NewPacketConn(conn)
	if err := packet.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst, true); err != nil {
		conn.Close()
		return err
	}
	if err := packet.JoinGroup(iface, &net.UDPAddr{IP: net.ParseIP("ff02::1:2")}); err != nil {
		conn.Close()
		return err
	}
	snapshot := dhcp6serverconfig.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, ServerDUID: hex.EncodeToString(server)}
	if err := report(ctx, snapshot); err != nil {
		conn.Close()
		return err
	}
	defer conn.Close()
	if ready != nil {
		ready()
	}
	in := initial
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	data, err := prepare(cfg, in, clock)
	if err != nil {
		return err
	}
	buffer := make([]byte, 65535)
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case updated, open := <-updates:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("DHCPv6 server input owner stopped")
			}
			if err := dhcp6serverconfig.ValidateInput(updated); err != nil {
				return err
			}
			changed := !slices.Equal(updated.DNSServers, in.DNSServers) || !slices.Equal(updated.SNTPServers, in.SNTPServers) || !slices.Equal(updated.DomainSearch, in.DomainSearch)
			in = updated
			if changed {
				clock, err = boottime.Read()
				if err != nil {
					return err
				}
				data, err = prepare(cfg, in, clock)
				if err != nil {
					return err
				}
			}
		default:
		}
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return err
		}
		n, control, from, err := packet.ReadFrom(buffer)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		peer := from.(*net.UDPAddr)
		if control == nil || control.IfIndex != iface.Index || peer.IP.IsUnspecified() || peer.IP.IsMulticast() {
			continue
		}
		r, err := decode(buffer[:n], server)
		if err != nil {
			continue
		}
		// Read BOOTTIME again after the receive wait: the prepared option
		// bytes must not cross their producer's original expiry.
		current, err := boottime.Read()
		if err != nil {
			return err
		}
		if !in.Authority.Live(current) {
			continue
		}
		if data.expires != 0 && uint64(current.BoottimeNanoseconds) >= data.expires {
			data, err = prepare(cfg, in, current)
			if err != nil {
				return err
			}
		}
		to := &net.UDPAddr{IP: peer.IP, Zone: cfg.Interface, Port: 546}
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		if _, err := packet.WriteTo(reply(r, server, data, cfg.RefreshSeconds), &ipv6.ControlMessage{IfIndex: iface.Index, Src: net.IP(source.AsSlice())}, to); err != nil {
			slog.Warn("send DHCPv6 information", "error", err)
		}
	}
}

type serverIdentity struct {
	duid []byte
	lock *os.File
}

func loadIdentity(ctx context.Context, cfg dhcp6serverconfig.Config) (serverIdentity, error) {
	dir, name, err := statefs.OpenParent(cfg.StatePath, true)
	if err != nil {
		return serverIdentity{}, err
	}
	defer dir.Close()
	lock, err := os.OpenFile(cfg.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return serverIdentity{}, err
	}
	var saved serverIdentity
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return saved, err
	}
	// Explicit identity is durable operator intent in the configuration journal.
	// Preserve the automatic identity across temporary overrides and rollback.
	if cfg.ServerDUID != "" {
		saved.duid, err = dhcp6config.ParseDUID(cfg.ServerDUID)
		if err != nil {
			return saved, err
		}
		saved.lock = lock
		ok = true
		return saved, nil
	}
	raw, err := statefs.ReadPrivate(ctx, dir, name, 256)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return saved, err
	}
	text := string(raw)
	if text != "" {
		saved.duid, err = dhcp6config.ParseDUID(text)
		if err != nil {
			return saved, err
		}
	} else {
		saved.duid = make([]byte, 18)
		saved.duid[1] = 4
		if _, err := rand.Read(saved.duid[2:]); err != nil {
			return saved, err
		}
		saved.duid[8] = saved.duid[8]&15 | 64
		saved.duid[10] = saved.duid[10]&63 | 128
	}
	text = hex.EncodeToString(saved.duid)
	if string(raw) != text {
		file, err := os.CreateTemp(filepath.Dir(cfg.StatePath), ".dhcp6-server-*")
		if err != nil {
			return saved, err
		}
		defer os.Remove(file.Name())
		_, writeErr := file.WriteString(text)
		err = errors.Join(writeErr, file.Sync(), file.Close())
		if err != nil {
			return saved, err
		}
		if err := os.Rename(file.Name(), cfg.StatePath); err != nil {
			return saved, err
		}
		if err := dir.Sync(); err != nil {
			return saved, err
		}
	}
	saved.lock = lock
	ok = true
	return saved, nil
}
