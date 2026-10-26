package runtimeplan

import (
	"bytes"
	"errors"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Decode once at the receiving boundary. The accepted typed observation feeds
// DNS, conditions and status; its original envelope remains available to the API.
func (r *Runtime) validateDHCPServerSnapshotLocked(b binding, u serviceconfig.Update) (dhcpserverconfig.Snapshot, error) {
	if len(u.Snapshot) > dhcpserverconfig.MaxSnapshotBytes {
		return dhcpserverconfig.Snapshot{}, errors.New("DHCP snapshot exceeds its IPC bound")
	}
	var cfg dhcpserverconfig.Config
	var s dhcpserverconfig.Snapshot
	if err := strict(b.instance.Config, &cfg); err != nil {
		return s, err
	}
	if err := strict(u.Snapshot, &s); err != nil {
		return s, err
	}
	if err := dhcpserverconfig.ValidateSnapshot(cfg, s); err != nil {
		return s, err
	}
	clock, err := boottime.Read()
	if err != nil {
		return s, err
	}
	if !dhcpserverconfig.CurrentObservation(s, clock) {
		return s, errors.New("DHCP server original observation expired or belongs to a different boot or future clock")
	}
	previous := r.records[u.Name]
	if previous.Generation != u.Generation || previous.ProducerEpoch != u.ProducerEpoch {
		return s, nil
	}
	old := r.dhcpServers[u.Name]
	if old.Epoch != s.Epoch || s.Sequence < old.Sequence || s.ObservedNanos < old.ObservedNanos || !old.Active && s.Active {
		return s, errors.New("DHCP server rewound its observation or final fence")
	}
	if old.Sequence == s.Sequence && !bytes.Equal(previous.Snapshot, u.Snapshot) || old.ObservedNanos == s.ObservedNanos && old.ExpiresNanos != s.ExpiresNanos {
		return s, errors.New("DHCP server reused an observation with changed contents or deadline")
	}
	return s, nil
}
