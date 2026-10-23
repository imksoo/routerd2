// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"golang.org/x/sys/unix"
)

const maxJournalBytes = 8 << 20

type journal struct {
	Version     int           `json:"version"`
	Resource    string        `json:"resource"`
	Interface   string        `json:"interface"`
	WrittenAt   time.Time     `json:"written_at"`
	Leases      []storedLease `json:"leases"`
	Quarantines []quarantine  `json:"quarantines,omitempty"`
}
type journalStore struct {
	path string
	lock *os.File
}

func privateParent(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("DHCP module files require a private 0700 parent directory")
	}
	// Reject symlink ancestors as well: an otherwise private final directory
	// must not redirect state into another service's ownership domain.
	for p := dir; p != "/"; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return errors.New("DHCP module path contains a symlink")
		}
	}
	return nil
}
func openStore(c Config, now time.Time) (*journalStore, journal, error) {
	if err := privateParent(c.StatePath); err != nil {
		return nil, journal{}, err
	}
	fd, err := unix.Open(c.StatePath+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, journal{}, err
	}
	f := os.NewFile(uintptr(fd), c.StatePath+".lock")
	fail := func(e error) (*journalStore, journal, error) { _ = f.Close(); return nil, journal{}, e }
	info, e := f.Stat()
	if e != nil {
		return fail(e)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fail(errors.New("DHCP journal lock must be a private regular file"))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("DHCP journal is already owned: %w", err))
	}
	store := &journalStore{path: c.StatePath, lock: f}
	data, exists, err := readPrivate(c.StatePath, maxJournalBytes)
	if err != nil {
		return fail(err)
	}
	j := journal{Version: 1, Resource: c.Resource, Interface: c.Interface, WrittenAt: now}
	if !exists {
		return store, j, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&j); err != nil {
		return fail(fmt.Errorf("decode DHCP lease journal: %w", err))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fail(errors.New("DHCP journal has trailing JSON"))
	}
	if j.Version != 1 || j.Resource != c.Resource || j.Interface != c.Interface {
		return fail(errors.New("DHCP journal owner/version differs; an explicit lease migration is required"))
	}
	if j.WrittenAt.After(now.Add(5 * time.Minute)) {
		return fail(errors.New("DHCP wall clock precedes its durable journal; synchronize time before serving leases"))
	}
	if len(j.Leases) > 2048 || len(j.Quarantines) > 2048 {
		return fail(errors.New("DHCP lease journal exceeds its record bound"))
	}
	ids, ips := map[string]bool{}, map[string]bool{}
	for _, l := range j.Leases {
		m, e := net.ParseMAC(l.MACAddress)
		if e != nil || len(m) != 6 || m[0]&1 != 0 {
			return fail(errors.New("DHCP journal has invalid hardware identity"))
		}
		if id, e := hex.DecodeString(l.ClientID); e != nil || len(id) > 255 || l.ClientID != "" && len(id) < 2 {
			return fail(errors.New("DHCP journal has invalid client identifier"))
		}
		if _, e := dhcpserverconfig.IPv4(l.IPAddress); e != nil {
			return fail(errors.New("DHCP journal has invalid leased address"))
		}
		if l.Scope == "" || len(l.Scope) > 64 || l.Hostname != "" && !dhcpserverconfig.ValidHostname(l.Hostname) || l.AcquiredAt.IsZero() || l.RenewedAt.Before(l.AcquiredAt) || l.ExpiresAt.Before(l.RenewedAt) || l.HeldUntil.Before(l.ExpiresAt) {
			return fail(errors.New("DHCP journal contains an invalid lease lifetime or scope"))
		}
		key := leaseKey(l.Lease)
		if ids[key] || ips[l.IPAddress] {
			return fail(errors.New("DHCP journal contains conflicting bindings"))
		}
		ids[key], ips[l.IPAddress] = true, true
	}
	qips := map[string]bool{}
	for _, q := range j.Quarantines {
		if _, e := dhcpserverconfig.IPv4(q.IPAddress); e != nil || q.Until.IsZero() || qips[q.IPAddress] || ips[q.IPAddress] {
			return fail(errors.New("DHCP journal contains invalid/conflicting quarantines"))
		}
		qips[q.IPAddress] = true
	}
	return store, j, nil
}
func (s *journalStore) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	return s.lock.Close()
}
func (s *journalStore) Save(j journal) error {
	sort.Slice(j.Leases, func(i, k int) bool { return leaseKey(j.Leases[i].Lease) < leaseKey(j.Leases[k].Lease) })
	sort.Slice(j.Quarantines, func(i, k int) bool { return j.Quarantines[i].IPAddress < j.Quarantines[k].IPAddress })
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(b) > maxJournalBytes {
		return errors.New("DHCP lease journal exceeds size bound")
	}
	return writePrivate(s.path, append(b, '\n'))
}
func readPrivate(path string, maxBytes int) ([]byte, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > int64(maxBytes) {
		return nil, false, errors.New("DHCP persisted data must be a bounded private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxBytes {
		return nil, false, errors.New("DHCP persisted data exceeds size limit")
	}
	return b, true, nil
}
func writePrivate(path string, data []byte) error {
	if err := privateParent(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("DHCP state destination must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
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
