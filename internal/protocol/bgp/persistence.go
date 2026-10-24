// SPDX-License-Identifier: BSD-3-Clause
package bgp

import (
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

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"golang.org/x/sys/unix"
)

const maxStateBytes = 16 << 20

type stateOwner struct {
	Resource string `json:"resource"`
	Digest   string `json:"committed_digest"`
}

// Embed the original applied record so the old HTTP clients and read helpers
// retain their JSON shape. Ownership and intent digest change in the same
// fsynced rename as the journal; a second sidecar could resurrect stale state.
type stateRecord struct {
	bgpdaemon.AppliedConfig
	Owner *stateOwner `json:"_routerd2,omitempty"`
}

func committedDigest(c Config) string {
	c = Normalize(c)
	c.Runtime.UpdatedAt = ""
	for i := range c.Runtime.Paths {
		c.Runtime.Paths[i].UUID = ""
	}
	// File locations and observation timeouts do not change routing intent.
	b, _ := json.Marshal(struct {
		Runtime       bgpdaemon.AppliedConfig
		Transports    map[string]PeerTransport
		FIBInterface  string
		BindInterface string
		Table         int
		Metric        int
		PeerBFD       map[string]bgpconfig.BFDReference `json:"PeerBFD,omitempty"`
	}{c.Runtime, c.PeerTransports, c.FIBInterface, c.BindInterface, c.Table, c.RouteMetric, c.PeerBFD})
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func readState(path string) (stateRecord, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return stateRecord{}, false, nil
	}
	if err != nil {
		return stateRecord{}, false, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return stateRecord{}, false, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxStateBytes || fi.Mode().Perm()&0077 != 0 {
		return stateRecord{}, false, errors.New("BGP applied state must be a private bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return stateRecord{}, false, err
	}
	if len(b) > maxStateBytes {
		return stateRecord{}, false, errors.New("BGP applied state exceeds size limit")
	}
	if strings.TrimSpace(string(b)) == "" {
		return stateRecord{}, false, nil
	}
	var rec stateRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return stateRecord{}, false, err
	}
	rec.AppliedConfig = bgpdaemon.Normalize(rec.AppliedConfig)
	return rec, true, nil
}

func readApplied(path string) (bgpdaemon.AppliedConfig, bool, error) {
	rec, ok, err := readState(path)
	return rec.AppliedConfig, ok, err
}

func writeApplied(path string, applied bgpdaemon.AppliedConfig) error {
	rec, _, err := readState(path)
	if err != nil {
		return err
	}
	rec.AppliedConfig = bgpdaemon.Normalize(applied)
	rec.Version = bgpdaemon.AppliedVersion
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return atomicJSON(path, rec)
}

func initializeState(c Config) error {
	rec, ok, err := readState(c.StatePath)
	if err != nil {
		return fmt.Errorf("read BGP applied journal: %w", err)
	}
	digest := committedDigest(c)
	if ok && rec.Owner != nil && rec.Owner.Resource == c.Resource && rec.Owner.Digest == digest {
		if err := bgpconfig.ValidateRuntime(rec.AppliedConfig); err != nil {
			return fmt.Errorf("invalid persisted BGP journal: %w", err)
		}
		return nil
	}
	return atomicJSON(c.StatePath, stateRecord{
		AppliedConfig: c.Runtime,
		Owner:         &stateOwner{Resource: c.Resource, Digest: digest},
	})
}

func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return errors.New("BGP JSON state exceeds size limit")
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return errors.New("refuse to replace a nonregular BGP state file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bgp-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func appendEvent(path string, event Event) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > 8192 {
		return errors.New("BGP event exceeds size limit")
	}
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
			return errors.New("BGP event path must be a private regular file")
		}
		if fi.Size()+int64(len(b)) > 1<<20 {
			if old, err := os.Lstat(path + ".1"); err == nil && !old.Mode().IsRegular() {
				return errors.New("refuse to replace nonregular BGP event archive")
			}
			if err := os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	_, err = f.Write(b)
	return err
}
