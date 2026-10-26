// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

func (d *daemon) applyLeaseProjection(raw dnsconfig.LeaseProjection) error {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	d.stateMu.RLock()
	cfg := d.config
	d.stateMu.RUnlock()
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	p := dnsconfig.NormalizeLeaseProjection(raw)
	if err := dnsconfig.ValidateLeaseProjection(cfg, p, clock); err != nil {
		return err
	}
	previous := make(map[string]dnsconfig.LeaseSet)
	if d.nativeLeases != nil {
		for _, set := range d.nativeLeases.Sets {
			previous[set.Zone+"/"+set.Source] = set
		}
	}
	for _, set := range p.Sets {
		old, ok := previous[set.Zone+"/"+set.Source]
		if !ok || old.Epoch != set.Epoch {
			continue
		}
		if set.Sequence < old.Sequence || set.ObservedNanos < old.ObservedNanos {
			return errors.New("stale DNS lease source observation")
		}
		if set.Sequence == old.Sequence && (!set.ObservedAt.Equal(old.ObservedAt) || set.ObservedNanos != old.ObservedNanos || set.ExpiresNanos != old.ExpiresNanos || set.BootID != old.BootID || !slices.Equal(set.Records, old.Records)) || set.ObservedNanos == old.ObservedNanos && set.ExpiresNanos != old.ExpiresNanos {
			return errors.New("DNS lease source reused an observation with changed contents or deadline")
		}
	}
	zones := newZoneTable(cfg.Zones)
	if zones.err != nil {
		return zones.err
	}
	d.nativeLeases = &p
	d.populateNativeZones(zones, cfg, clock)
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
// allowlist and retains the original publisher boot deadlines.
func (d *daemon) populateNativeZones(table *zoneTable, cfg dnsresolver.RuntimeConfig, clock boottime.Clock) {
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
		if !allowed[key] || set.BootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= set.ExpiresNanos {
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
				if !clock.WallTime.Before(lease.ExpiresAt) {
					continue
				}
				record := zoneRecord{Hostname: zone.recordName(lease.Hostname), IPv4: []string{lease.Address}, TTL: ttl, Source: zoneRecordSourceTypedLease, ExpiresAt: lease.ExpiresAt, BootID: set.BootID, ExpiresNanos: set.ExpiresNanos}
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
