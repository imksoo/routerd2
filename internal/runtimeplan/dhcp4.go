package runtimeplan

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func validateDHCP4Transition(previous, next serviceconfig.Update) error {
	var s dhcp4.Snapshot
	if err := json.Unmarshal(next.Snapshot, &s); err != nil {
		return err
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if s.BootID != clock.BootID || s.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return errors.New("DHCPv4 observation belongs to another boot or future clock")
	}
	if previous.Generation != next.Generation || previous.ProducerEpoch != next.ProducerEpoch || len(previous.Snapshot) == 0 {
		return nil
	}
	var old dhcp4.Snapshot
	if err := json.Unmarshal(previous.Snapshot, &old); err != nil {
		return err
	}
	if old.BootID != s.BootID || old.Epoch != s.Epoch || s.Sequence < old.Sequence || s.ObservedNanos < old.ObservedNanos || old.Stopped && !s.Stopped || old.HardwareAddress != s.HardwareAddress || !bytes.Equal(old.ClientIdentity, s.ClientIdentity) {
		return errors.New("DHCPv4 observation rewound or crossed final fence")
	}
	if old.Sequence == s.Sequence && !bytes.Equal(previous.Snapshot, next.Snapshot) && !s.Stopped {
		return errors.New("DHCPv4 observation time reused with different content")
	}
	if old.LeaseClock == s.LeaseClock && s.LeaseClock != (dhcp4.LeaseClock{}) {
		a, b := old.Snapshot, s.Snapshot
		a.State, b.State = "", ""
		a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
		a.LastError, b.LastError = "", ""
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		if !bytes.Equal(left, right) {
			return errors.New("DHCPv4 original lease reused with different payload")
		}
	} else if s.Active && old.LeaseClock.BootID == s.LeaseClock.BootID && (old.LeaseClock.AcquiredNanos > s.LeaseClock.AcquiredNanos || old.LeaseClock.Epoch == s.Epoch && old.LeaseClock.Sequence >= s.LeaseClock.Sequence) {
		return errors.New("DHCPv4 lease changed without a newer server ACK")
	}
	return nil
}
