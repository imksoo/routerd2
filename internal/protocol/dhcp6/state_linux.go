//go:build linux

package dhcp6

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/statefs"
)

const stateVersion = 1

type checkpoint struct {
	Version  int                  `json:"version"`
	Snapshot dhcp6config.Snapshot `json:"snapshot"`
}

func readState(ctx context.Context, cfg dhcp6config.Config) (dhcp6config.Snapshot, error) {
	dir, err := os.Open(filepath.Dir(cfg.StatePath))
	if err != nil {
		return dhcp6config.Snapshot{}, err
	}
	defer dir.Close()
	raw, err := statefs.ReadPrivate(ctx, dir, filepath.Base(cfg.StatePath), 2<<20)
	if errors.Is(err, os.ErrNotExist) {
		return dhcp6config.Snapshot{}, nil
	}
	if err != nil {
		return dhcp6config.Snapshot{}, err
	}
	var saved checkpoint
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return dhcp6config.Snapshot{}, err
	}
	var trailing json.RawMessage
	if d.Decode(&trailing) != io.EOF || saved.Version != stateVersion {
		return dhcp6config.Snapshot{}, errors.New("unsupported or trailing DHCPv6 checkpoint")
	}
	s := saved.Snapshot
	if s.Resource != cfg.Resource {
		return dhcp6config.Snapshot{}, errors.New("DHCPv6 checkpoint belongs to another resource")
	}
	// Validate the stored facts independently of changed operator intent.
	previous := dhcp6config.Config{Resource: s.Resource, Interface: s.Interface, IAID: s.IAID, RequestAddress: len(s.Addresses) > 0, RequestPrefix: len(s.Prefixes) > 0}
	if err := dhcp6config.ValidateSnapshot(previous, s); err != nil {
		return dhcp6config.Snapshot{}, err
	}
	return s, nil
}

func (c *client) restorePD(saved dhcp6config.Snapshot, clock boottime.Clock) (bool, error) {
	if !c.cfg.RequestPrefix || saved.BootID != clock.BootID || saved.Interface != c.cfg.Interface || saved.IAID != c.cfg.IAID || saved.ClientDUID != hex.EncodeToString(c.duid) || saved.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return false, nil
	}
	saved.Prefixes = slices.DeleteFunc(slices.Clone(saved.Prefixes), func(p dhcp6config.Prefix) bool { return !p.ValidUntil.Live(clock) })
	if len(saved.Prefixes) == 0 {
		return false, nil
	}
	saved.Addresses = slices.DeleteFunc(slices.Clone(saved.Addresses), func(a dhcp6config.Address) bool { return !c.cfg.RequestAddress || !a.ValidUntil.Live(clock) })
	c.snapshot = saved
	var err error
	c.server, err = hex.DecodeString(saved.ServerDUID)
	if err != nil {
		return false, err
	}
	now := time.Unix(0, clock.BoottimeNanoseconds)
	if err := c.begin(rebind, now); err != nil {
		return false, err
	}
	// RFC 8415 18.2.12 verifies persisted PD with Rebind using Confirm's
	// bounded retry parameters. Neither recovery nor silence renews a lease.
	c.lease.next = now.Add(time.Duration(mathrand.Int64N(int64(time.Second))))
	c.lease.verifyUntil = now.Add(10 * time.Second)
	if saved.InformationRefreshAt.Nanos != 0 {
		c.info.next = time.Unix(0, int64(saved.InformationRefreshAt.Nanos))
	}
	return true, nil
}

func writeState(path string, s dhcp6config.Snapshot) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp6-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(checkpoint{Version: stateVersion, Snapshot: s})
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
