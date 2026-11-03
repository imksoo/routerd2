package runtimeplan

import (
	"bytes"
	"errors"
	"net/netip"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// One typed receive view feeds DNS, conditions and status. The full protocol
// envelope stays available to the API without another lease database.
type leaseObservation struct {
	Epoch                       string
	Sequence                    uint64
	Active                      bool
	ObservedAt                  time.Time
	BootID                      string
	ObservedNanos, ExpiresNanos uint64
	Records                     []dnsconfig.LeaseRecord
}

func (s leaseObservation) current(clock boottime.Clock) bool {
	now := uint64(clock.BoottimeNanoseconds)
	return s.BootID == clock.BootID && s.ObservedNanos > 0 && s.ObservedNanos <= now && now < s.ExpiresNanos
}

func (r *Runtime) validateDHCPLeaseSnapshotLocked(b binding, u serviceconfig.Update) (leaseObservation, error) {
	var out leaseObservation
	if len(u.Snapshot) > dhcpserverconfig.MaxSnapshotBytes {
		return out, errors.New("DHCP snapshot exceeds its IPC bound")
	}
	clock, err := boottime.Read()
	if err != nil {
		return out, err
	}
	switch u.Role {
	case "dhcp_server":
		var cfg dhcpserverconfig.Config
		var s dhcpserverconfig.Snapshot
		if err := strict(b.instance.Config, &cfg); err != nil {
			return out, err
		}
		if err := strict(u.Snapshot, &s); err != nil {
			return out, err
		}
		if err := dhcpserverconfig.ValidateSnapshot(cfg, s); err != nil {
			return out, err
		}
		if !dhcpserverconfig.CurrentObservation(s, clock) {
			return out, errors.New("DHCP server original observation expired or belongs to a different boot or future clock")
		}
		out = leaseObservation{Epoch: s.Epoch, Sequence: s.Sequence, Active: s.Active, ObservedAt: s.ObservedAt, BootID: s.BootID, ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ExpiresNanos}
		for _, l := range s.Leases {
			if dnsconfig.ValidLeaseHostname(l.Hostname) {
				out.Records = append(out.Records, dnsconfig.LeaseRecord{Hostname: l.Hostname, Address: netip.MustParseAddr(l.IPAddress), ExpiresAt: l.ExpiresAt})
			}
		}
	case "dhcp6_server":
		var cfg dhcp6serverconfig.Config
		var s dhcp6serverconfig.Snapshot
		if err := strict(b.instance.Config, &cfg); err != nil {
			return out, err
		}
		if err := strict(u.Snapshot, &s); err != nil {
			return out, err
		}
		if err := dhcp6serverconfig.ValidateSnapshot(cfg, s, clock); err != nil {
			return out, err
		}
		out = leaseObservation{Epoch: u.ProducerEpoch, Sequence: u.Sequence, Active: s.Active, ObservedAt: s.ObservedAt, BootID: s.BootID, ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ExpiresNanos}
		for _, l := range s.Leases {
			if dnsconfig.ValidLeaseHostname(l.Hostname) {
				out.Records = append(out.Records, dnsconfig.LeaseRecord{Hostname: l.Hostname, Address: l.Address, ExpiresAt: l.ExpiresAt})
			}
		}
	default:
		return out, errors.New("not a DHCP lease producer")
	}
	previous := r.records[u.Name]
	if previous.Generation != u.Generation || previous.ProducerEpoch != u.ProducerEpoch {
		return out, nil
	}
	old := r.leaseSources[u.Name]
	if old.Epoch != out.Epoch || out.Sequence < old.Sequence || out.ObservedNanos < old.ObservedNanos {
		return out, errors.New("DHCP server rewound its observation")
	}
	if old.Sequence == out.Sequence && !bytes.Equal(previous.Snapshot, u.Snapshot) || old.ObservedNanos == out.ObservedNanos && old.ExpiresNanos != out.ExpiresNanos {
		return out, errors.New("DHCP server reused an observation with changed contents or deadline")
	}
	return out, nil
}
