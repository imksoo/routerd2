// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"errors"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

func (d *daemon) applyInput(raw dnsconfig.Input) error {
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	d.stateMu.RLock()
	cfg := d.config
	d.stateMu.RUnlock()
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	p := dnsconfig.NormalizeInput(raw)
	if err := dnsconfig.ValidateInput(cfg, p, clock); err != nil {
		return err
	}
	if err := dnsconfig.ValidateListenInput(d.listenSources, p.Listeners, clock); err != nil {
		return err
	}
	previous := make(map[string]dnsconfig.LeaseSet)
	if d.nativeInput != nil {
		for _, set := range d.nativeInput.Sets {
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
	listenersChanged := d.nativeInput == nil || !slices.Equal(d.nativeInput.Listeners, p.Listeners)

	d.stateMu.RLock()
	zones := d.zones
	d.stateMu.RUnlock()
	leaseUpdate := len(p.Sets)+len(p.Records) > 0 || d.nativeInput != nil && len(d.nativeInput.Sets)+len(d.nativeInput.Records) > 0
	if leaseUpdate {
		zones = newZoneTable(cfg.Zones)
	}
	// Only input changes under this lock; configured pools and listeners retain
	// their lifetimes while learned server ownership changes.
	d.stateMu.RLock()
	sources := append([]runtimeSource(nil), d.sources...)
	d.stateMu.RUnlock()
	for _, source := range sources {
		if source.Pool == nil {
			continue
		}
		var sets []dnsconfig.LearnedUpstream
		for _, set := range p.Upstreams {
			if set.Source == source.Spec.Name {
				sets = append(sets, set)
			}
		}
		var bootstrap []dnsconfig.LearnedUpstream
		for _, server := range p.Bootstraps {
			if server.Source == source.Spec.Name {
				bootstrap = append(bootstrap, server)
			}
		}
		source.Pool.setLearned(sets, bootstrap)
	}
	changed := d.nativeInput == nil || !sameInputContent(*d.nativeInput, p)
	d.nativeInput = &p
	if leaseUpdate {
		d.populateNativeZones(zones, clock)
	}
	d.stateMu.Lock()
	d.zones = zones
	if changed {
		d.generation++
	}
	d.stateMu.Unlock()
	if changed {
		// An ownership heartbeat alone must not evict the forwarding cache.
		// Changed bindings must invalidate a cached upstream PTR/NXDOMAIN.
		d.mu.Lock()
		clear(d.cache)
		d.cacheBytes = 0
		d.mu.Unlock()
	}
	if listenersChanged {
		return d.updateListeners(p.Listeners, clock)
	}
	return nil
}

// Caller holds inputMu. Source authorization was checked for this complete
// input; the new zone table retains the original publisher deadlines.
func (d *daemon) populateNativeZones(table *zoneTable, clock boottime.Clock) {
	if d.nativeInput == nil {
		return
	}
	for _, address := range d.nativeInput.Records {
		if !address.ValidUntil.Live(clock) {
			continue
		}
		declaredZone := d.config.Zones[address.Zone]
		declared := declaredZone.Spec.Records[address.Record]
		zone := table.zones[strings.ToLower(dns.Fqdn(firstNonEmpty(declaredZone.Spec.Zone, declaredZone.Name)))]
		name := zone.recordName(declared.Hostname)
		record := zone.Records[name]
		if address.Address.Is4() {
			record.IPv4 = append(record.IPv4, address.Address.String())
		} else {
			record.IPv6 = append(record.IPv6, address.Address.String())
		}
		if record.AddressLifetimes == nil {
			record.AddressLifetimes = make(map[string]lifetime.Deadline)
		}
		record.AddressLifetimes[address.Address.String()] = address.ValidUntil
		zone.mergeRecord(record)
	}
	for _, set := range d.nativeInput.Sets {
		if uint64(clock.BoottimeNanoseconds) >= set.ExpiresNanos {
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

// Heartbeat deadlines change without changing answers or cached RR content.
func sameInputContent(a, b dnsconfig.Input) bool {
	return slices.EqualFunc(a.Records, b.Records, func(a, b dnsconfig.RecordAddress) bool {
		return a.Zone == b.Zone && a.Record == b.Record && a.Address == b.Address
	}) && slices.EqualFunc(a.Listeners, b.Listeners, func(a, b dnsconfig.ListenAddress) bool { return a.Source == b.Source && a.Address == b.Address }) && slices.EqualFunc(a.Sets, b.Sets, func(a, b dnsconfig.LeaseSet) bool {
		return a.Zone == b.Zone && a.Source == b.Source && slices.Equal(a.Records, b.Records)
	}) && sameServers(a.Upstreams, b.Upstreams) && sameServers(a.Bootstraps, b.Bootstraps)
}

func sameServers(a, b []dnsconfig.LearnedUpstream) bool {
	return slices.EqualFunc(a, b, func(a, b dnsconfig.LearnedUpstream) bool {
		return a.Source == b.Source && a.Provider == b.Provider && a.Address == b.Address
	})
}
