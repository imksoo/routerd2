package runtimeplan

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func validateVRRPTransition(previous, next serviceconfig.Update) error {
	var s vrrp.Snapshot
	if err := json.Unmarshal(next.Snapshot, &s); err != nil {
		return err
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if s.BootID != clock.BootID || s.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return errors.New("VRRP observation belongs to another boot or future clock")
	}
	if previous.Generation != next.Generation || previous.ProducerEpoch != next.ProducerEpoch || len(previous.Snapshot) == 0 {
		return nil
	}
	var old vrrp.Snapshot
	if err := json.Unmarshal(previous.Snapshot, &old); err != nil {
		return err
	}
	if old.Epoch != s.Epoch || old.BootID != s.BootID || s.ObservedNanos < old.ObservedNanos || s.Transition < old.Transition || old.State == vrrp.StateInitialize && s.State != vrrp.StateInitialize || s.Transition == old.Transition && s.State != old.State {
		return errors.New("VRRP observation crossed its epoch, transition or terminal fence")
	}
	if s.ObservedNanos == old.ObservedNanos && !bytes.Equal(previous.Snapshot, next.Snapshot) && s.State != vrrp.StateInitialize {
		return errors.New("VRRP original observation reused with different content")
	}
	if s.Active && old.Active && s.Transition == old.Transition && uint64(clock.BoottimeNanoseconds) >= old.ExpiresNanos {
		return errors.New("expired VRRP election requires a new transition")
	}
	return nil
}

func (r *Runtime) recordVRRPOwnershipLocked(u serviceconfig.Update) error {
	var s vrrp.Snapshot
	if err := json.Unmarshal(u.Snapshot, &s); err != nil {
		return err
	}
	if !s.Active {
		delete(r.leases, u.Name)
		return nil
	}
	clock, err := boottime.Read()
	if err != nil || !vrrp.CurrentOwnership(s, clock) {
		return errors.Join(errors.New("invalid or expired VRRP boot ownership"), err)
	}
	if previous, ok := r.records[u.Name]; ok && previous.Generation == u.Generation && previous.ProducerEpoch == u.ProducerEpoch {
		var old vrrp.Snapshot
		if json.Unmarshal(previous.Snapshot, &old) == nil && old.BootID == s.BootID && old.Epoch == s.Epoch && old.ObservedNanos == s.ObservedNanos && old.ExpiresNanos == s.ExpiresNanos && !r.leases[u.Name].IsZero() {
			return nil // Transport replay never renews the original local receipt.
		}
	}
	r.leases[u.Name] = time.Now().Add(time.Duration(s.ExpiresNanos - uint64(clock.BoottimeNanoseconds)))
	return nil
}
