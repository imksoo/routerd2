//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/boottime"

	"golang.org/x/sys/unix"
)

const maxStateBytes = 1 << 20

type journal struct {
	Version  int      `json:"version"`
	Identity string   `json:"identity"`
	Snapshot Snapshot `json:"snapshot"`
}

func (s *state) restore(path string, now boottime.Clock) error {
	record, exists, err := readState(path)
	if err != nil || !exists {
		return err
	}
	return s.restoreRecord(record, now)
}

func (s *state) restoreRecord(record journal, now boottime.Clock) error {
	if record.Version != 2 || record.Identity == "" {
		return errors.New("invalid RA state journal envelope")
	}
	if record.Snapshot.BootID != now.BootID {
		s.learned.LastError = "stored RA learning discarded after reboot; waiting for fresh advertisements"
		return nil
	}
	if record.Identity != s.identity {
		s.learned.LastError = "stored RA learning discarded after interface/config identity change"
		return nil
	}
	if err := ValidateSnapshot(s.config, record.Snapshot); err != nil {
		return fmt.Errorf("invalid RA state snapshot: %w", err)
	}
	if record.Snapshot.ObservedNanos > uint64(now.BoottimeNanoseconds) {
		return errors.New("RA state journal has a future boot timestamp")
	}
	s.learned = clone(record.Snapshot)
	s.learned.Active = true
	expireSnapshot(&s.learned, now)
	return nil
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
