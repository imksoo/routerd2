//go:build linux

package dhcp6

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/imksoo/routerd2/internal/lifetime"
	"io"
	"math"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/statefs"
)

const stateVersion = 2

type clockMapping struct {
	LocalBootID string `json:"local_boot_id"`
	LocalNanos  uint64 `json:"local_nanos"`
	SourceNanos uint64 `json:"source_nanos"`
}

type checkpoint struct {
	Epoch    uint64               `json:"epoch,omitempty"`
	Source   string               `json:"source,omitempty"`
	Revision uint64               `json:"revision,omitempty"`
	Mapping  *clockMapping        `json:"mapping,omitempty"`
	Version  int                  `json:"version"`
	Snapshot dhcp6config.Snapshot `json:"snapshot"`
}

func readState(ctx context.Context, cfg dhcp6config.Config) (checkpoint, error) {
	dir, err := os.Open(filepath.Dir(cfg.StatePath))
	if err != nil {
		return checkpoint{}, err
	}
	defer dir.Close()
	raw, err := statefs.ReadPrivate(ctx, dir, filepath.Base(cfg.StatePath), 2<<20)
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint{}, nil
	}
	if err != nil {
		return checkpoint{}, err
	}
	return decodeState(cfg, raw)
}

func decodeState(cfg dhcp6config.Config, raw []byte) (checkpoint, error) {
	var saved checkpoint
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return checkpoint{}, err
	}
	var trailing json.RawMessage
	if d.Decode(&trailing) != io.EOF || saved.Version != stateVersion {
		return checkpoint{}, errors.New("unsupported or trailing DHCPv6 checkpoint")
	}
	s := saved.Snapshot
	if s.Resource != cfg.Resource {
		return checkpoint{}, errors.New("DHCPv6 checkpoint belongs to another resource")
	}
	// Validate the stored facts independently of changed operator intent.
	previous := dhcp6config.Config{Resource: s.Resource, Interface: s.Interface, IAID: s.IAID, RequestAddress: len(s.Addresses) > 0, RequestPrefix: len(s.Prefixes) > 0}
	if err := dhcp6config.ValidateSnapshot(previous, s); err != nil {
		return checkpoint{}, err
	}
	if saved.Mapping != nil {
		m := saved.Mapping
		if !boottime.ValidBootID(m.LocalBootID) || m.LocalNanos == 0 || m.LocalNanos > math.MaxInt64 || m.SourceNanos < s.ObservedNanos || m.SourceNanos > math.MaxInt64 {
			return checkpoint{}, errors.New("invalid DHCPv6 replica clock mapping")
		}
	}
	return saved, nil
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

func writeState(path string, s checkpoint) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dhcp6-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s)
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

// localSnapshot derives local deadlines once from the recorded pull exchange.
// The source snapshot remains unchanged in standby storage, so identical
// revisions never get a fresh receive-time budget.
func (s checkpoint) localSnapshot(clock boottime.Clock, iface string) dhcp6config.Snapshot {
	out := s.Snapshot
	if s.Mapping == nil {
		return out
	}
	m := s.Mapping
	if m.LocalBootID != clock.BootID {
		return dhcp6config.Snapshot{ClientDUID: out.ClientDUID}
	}
	stamp := func(n uint64) uint64 {
		delta := int64(n) - int64(m.SourceNanos)
		if delta > math.MaxInt64-int64(m.LocalNanos) {
			return math.MaxInt64
		}
		return uint64(max(1, int64(m.LocalNanos)+delta))
	}
	deadline := func(d lifetime.Deadline) lifetime.Deadline {
		if d == (lifetime.Deadline{}) {
			return d
		}
		d.BootID = m.LocalBootID
		if d.Nanos != 0 {
			d.Nanos = stamp(d.Nanos)
		}
		return d
	}
	out.BootID = m.LocalBootID
	out.Interface = iface
	out.ObservedNanos = stamp(out.ObservedNanos)
	out.Addresses = slices.Clone(out.Addresses)
	for i := range out.Addresses {
		a := &out.Addresses[i]
		a.PreferredUntil = deadline(a.PreferredUntil)
		a.ValidUntil = deadline(a.ValidUntil)
	}
	out.Prefixes = slices.Clone(out.Prefixes)
	for i := range out.Prefixes {
		p := &out.Prefixes[i]
		p.PreferredUntil = deadline(p.PreferredUntil)
		p.ValidUntil = deadline(p.ValidUntil)
	}
	out.RenewAt = deadline(out.RenewAt)
	out.RebindAt = deadline(out.RebindAt)
	if out.InfoReceivedNanos != 0 {
		out.InfoReceivedNanos = stamp(out.InfoReceivedNanos)
	}
	out.InformationRefreshAt = deadline(out.InformationRefreshAt)
	return out
}
