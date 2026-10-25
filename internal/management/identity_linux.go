//go:build linux

package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// after is a private test-only interruption hook, never operator input. A final
// name appears only after complete contents have been synced, without replace.
func publishHostIdentity(ctx context.Context, dir *os.File, name string, raw []byte, after func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return err
	}
	temporary := ".ssh-host-key-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "private-host-identity")
	defer f.Close()
	defer unix.Unlinkat(int(dir.Fd()), temporary, 0)
	check := func(stage string) error {
		if after != nil {
			if err := after(stage); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := check("created"); err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err := check("written"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := check("file-synced"); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(int(dir.Fd()), temporary, int(dir.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := check("published"); err != nil {
		return errors.Join(ErrIdentityUncertain, err)
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(ErrIdentityUncertain, err)
	}
	if err := check("directory-synced"); err != nil {
		return errors.Join(ErrIdentityUncertain, err)
	}
	return nil
}
