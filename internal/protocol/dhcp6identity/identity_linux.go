//go:build linux

// Package dhcp6identity keeps automatic identity separate from configured DUIDs
// and from leases. Callers own the directory and serialize writes with their
// protocol identity lock.
package dhcp6identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"

	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/sys/unix"
)

// Load uses configured intent without overwriting the automatic identity.
// A new Ethernet client starts with DUID-LL; other callers use DUID-UUID.
// Saved identity wins over a later link address change.
func Load(ctx context.Context, dir *os.File, name, configured string, hardware net.HardwareAddr) ([]byte, error) {
	if configured != "" {
		return dhcp6config.ParseDUID(configured)
	}
	raw, err := statefs.ReadPrivate(ctx, dir, name, 256)
	if err == nil {
		return dhcp6config.ParseDUID(string(raw))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var duid []byte
	if len(hardware) == 6 {
		duid = append([]byte{0, 3, 0, 1}, hardware...)
	} else {
		duid = make([]byte, 18)
		duid[1] = 4
		if _, err := rand.Read(duid[2:]); err != nil {
			return nil, err
		}
		duid[8] = duid[8]&15 | 64
		duid[10] = duid[10]&63 | 128
	}
	temporary := "." + name + "-" + rand.Text()
	fd, err := unix.Openat(int(dir.Fd()), temporary, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), temporary)
	defer unix.Unlinkat(int(dir.Fd()), temporary, 0)
	_, writeErr := file.WriteString(hex.EncodeToString(duid))
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return nil, err
	}
	if err := unix.Renameat(int(dir.Fd()), temporary, int(dir.Fd()), name); err != nil {
		return nil, err
	}
	if err := dir.Sync(); err != nil {
		return nil, err
	}
	return duid, nil
}
