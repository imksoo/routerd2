// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

type leaseReceipt struct {
	epoch    string
	sequence uint64
	deadline time.Time
}

func (d *daemon) applyLeaseProjection(raw dnsconfig.LeaseProjection) error {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	d.stateMu.RLock()
	cfg := d.config
	d.stateMu.RUnlock()
	now := time.Now()
	if err := dnsconfig.ValidateLeaseProjection(cfg, raw, now); err != nil {
		return err
	}
	p := dnsconfig.NormalizeLeaseProjection(raw)
	receipts := make(map[string]leaseReceipt, len(p.Sets))
	for _, set := range p.Sets {
		key := set.Zone + "/" + set.Source
		previous := d.leaseReceipts[key]
		if previous.epoch == set.Epoch {
			if set.Sequence < previous.sequence {
				return errors.New("stale DNS lease source sequence")
			}
			if set.Sequence == previous.sequence {
				receipts[key] = previous
				continue
			}
		}
		remaining := set.ValidUntil.Sub(now)
		if remaining > 5*time.Second {
			remaining = 5 * time.Second
		}
		receipts[key] = leaseReceipt{epoch: set.Epoch, sequence: set.Sequence, deadline: now.Add(remaining)}
	}
	zones := newZoneTable(cfg.Zones)
	if zones.err != nil {
		return zones.err
	}
	d.nativeLeases, d.leaseReceipts = &p, receipts
	d.populateNativeZones(zones, cfg, now)
	fingerprint := leaseContentFingerprint(p)
	d.stateMu.Lock()
	d.zones = zones
	if fingerprint != d.leaseFingerprint {
		d.generation++
	}
	d.stateMu.Unlock()
	if fingerprint != d.leaseFingerprint {
		// An ownership heartbeat alone must not evict the forwarding cache.
		// Changed bindings must invalidate a cached upstream PTR/NXDOMAIN.
		d.mu.Lock()
		clear(d.cache)
		d.cacheBytes = 0
		d.mu.Unlock()
		d.leaseFingerprint = fingerprint
	}
	return nil
}

// Caller holds reloadMu; table is newly built and not yet published. Applying
// the same projection during a configuration reload honors the new source
// allowlist and reuses the original monotonic receipt deadlines.
func (d *daemon) populateNativeZones(table *zoneTable, cfg dnsresolver.RuntimeConfig, now time.Time) {
	if d.nativeLeases == nil {
		return
	}
	allowed := map[string]bool{}
	for _, zone := range cfg.Zones {
		for _, ref := range zone.Spec.DHCPDerived.Sources {
			if name, ok := dnsconfig.LeaseSourceName(ref); ok {
				allowed[zone.Name+"/"+name] = true
			}
		}
	}
	for _, set := range d.nativeLeases.Sets {
		key := set.Zone + "/" + set.Source
		ownership := d.leaseReceipts[key].deadline
		if !allowed[key] || !now.Before(ownership) || !now.Before(set.ValidUntil) {
			continue
		}
		for _, zone := range table.zones {
			if zone.ResourceName != set.Zone {
				continue
			}
			ttl := uint32(zone.DHCP.TTL)
			if ttl == 0 {
				ttl = 60
			}
			for _, lease := range set.Records {
				if !now.Before(lease.ExpiresAt) {
					continue
				}
				record := zoneRecord{Hostname: zone.recordName(lease.Hostname), IPv4: []string{lease.Address}, TTL: ttl, Source: zoneRecordSourceTypedLease, ExpiresAt: lease.ExpiresAt, OwnershipUntil: ownership}
				// Static aliases and PTR ownership have higher precedence.
				zone.mergeRecord(record)
			}
		}
	}
}

func leaseContentFingerprint(p dnsconfig.LeaseProjection) string {
	type entry struct {
		Zone, Source string
		Records      []dnsconfig.LeaseRecord
	}
	entries := make([]entry, 0, len(p.Sets))
	for _, set := range p.Sets {
		entries = append(entries, entry{Zone: set.Zone, Source: set.Source, Records: set.Records})
	}
	b, _ := json.Marshal(entries)
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}
