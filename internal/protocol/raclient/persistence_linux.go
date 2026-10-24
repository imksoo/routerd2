//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const maxStateBytes = 1 << 20

type journal struct {
	Version       int       `json:"version"`
	Identity      string    `json:"identity"`
	BootID        string    `json:"boot_id"`
	SavedAt       time.Time `json:"saved_at"`
	SavedBootTime time.Time `json:"saved_boot_time"`
	Snapshot      Snapshot  `json:"snapshot"`
}

func (s *state) identity() string {
	c := s.config
	c.StatePath = ""
	b, _ := json.Marshal(struct {
		Config                Config
		InterfaceIndex        int
		Hardware, Source, IID string
	}{c, s.link.index, s.link.hardware.String(), s.link.source.String(), suffixString(s.iid)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *state) record(bootID string, now instant) journal {
	snapshot := s.snapshot(now)
	return journal{Version: 1, Identity: s.identity(), BootID: bootID, SavedAt: snapshot.UpdatedAt, SavedBootTime: now.boot, Snapshot: snapshot}
}

func (s *state) restore(path, bootID string, now instant) error {
	record, exists, err := readState(path)
	if err != nil || !exists {
		return err
	}
	return s.restoreRecord(record, bootID, now)
}

func (s *state) restoreRecord(record journal, bootID string, now instant) error {
	if record.Version != 1 || record.Identity == "" || record.BootID == "" || record.SavedAt.IsZero() || record.SavedBootTime.IsZero() || !record.Snapshot.UpdatedAt.Equal(record.SavedAt) {
		return errors.New("invalid RA state journal envelope")
	}
	if record.BootID != bootID {
		s.learned.LastError = "stored RA learning discarded after reboot; waiting for fresh advertisements"
		return nil
	}
	if record.Identity != s.identity() {
		s.learned.LastError = "stored RA learning discarded after interface/config identity change"
		return nil
	}
	if record.SavedBootTime.After(now.boot) || record.SavedBootTime.Before(time.Unix(0, 0)) {
		return errors.New("RA state journal has an invalid monotonic timestamp")
	}
	if err := ValidateSnapshot(s.config, record.Snapshot); err != nil {
		return fmt.Errorf("invalid RA state snapshot: %w", err)
	}
	next := clone(record.Snapshot)
	// This uses the stored boot clock, not elapsed wall time. It remains valid
	// across backward and forward NTP steps during a same-boot worker restart.
	convertDeadlines(&next, record.SavedAt, record.SavedBootTime)
	next.Active = true
	expireSnapshot(&next, now.boot)
	s.learned = next
	// Preserve the exact original UTC mapping across same-boot restart. A real
	// wall-clock step is still detected by snapshot/worker against this pair.
	s.clock = instant{wall: record.SavedAt, boot: record.SavedBootTime}
	return nil
}

func readBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if len(id) != 36 {
		return "", errors.New("kernel boot identity is unavailable")
	}
	return id, nil
}

func lockState(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path+".lock")
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
		err = errors.New("RA state lock must be a private regular file")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("own RA state: %w", err)
	}
	return f, nil
}

func readState(path string) (journal, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return journal{}, false, nil
	}
	if err != nil {
		return journal{}, false, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return journal{}, false, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > maxStateBytes {
		return journal{}, false, errors.New("RA state must be a private bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return journal{}, false, err
	}
	if len(b) > maxStateBytes {
		return journal{}, false, errors.New("RA state exceeds size limit")
	}
	var record journal
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&record); err != nil {
		return journal{}, false, errors.New("RA state is malformed")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return journal{}, false, errors.New("RA state has trailing content")
	}
	return record, true, nil
}

func saveState(path string, record journal) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ra-client-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(record)
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
