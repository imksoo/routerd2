// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/boottime"
)

type zoneTable struct {
	mu    sync.RWMutex
	zones map[string]*zoneData
	err   error
}

type zoneData struct {
	ResourceName string
	Name         string
	TTL          uint32
	DHCP         api.DNSZoneDHCPDerivedSpec
	DNSSEC       api.DNSZoneDNSSECSpec
	Records      map[string]zoneRecord
	PTR          map[string]string
	ReverseZone  map[string]bool
}

type zoneRecord struct {
	Hostname string
	IPv4     []string
	IPv6     []string
	TTL      uint32
	Source   zoneRecordSource
	// Native leases retain client UTC expiry and the original server boot
	// deadline. Query, polling and reload never extend either.
	ExpiresAt    time.Time
	BootID       string
	ExpiresNanos uint64
}

type zoneRecordSource uint8

const (
	// Numeric order is the reload merge priority. Do not reorder without
	// updating CopyDynamicFrom's precedence contract.
	zoneRecordSourceOldSticky       zoneRecordSource = 10
	zoneRecordSourceStickySnapshot  zoneRecordSource = 20
	zoneRecordSourceActiveEvent     zoneRecordSource = 30
	zoneRecordSourceActiveLeaseFile zoneRecordSource = 40
	zoneRecordSourceTypedLease      zoneRecordSource = 46
	zoneRecordSourceStatic          zoneRecordSource = 50

	// A newly received event is newer than the lease file loaded into the
	// current table, but becomes a retained event below the newly read lease
	// file when it crosses a reload boundary.
	zoneRecordPriorityLiveActiveEvent zoneRecordSource = 45
)

type dhcpLeaseEvent struct {
	Action   string `json:"action"`
	MAC      string `json:"mac,omitempty"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
}

func newZoneTable(zones []dnsresolver.RuntimeZone) *zoneTable {
	table := &zoneTable{zones: map[string]*zoneData{}}
	for _, runtime := range zones {
		spec := runtime.Spec
		name := strings.ToLower(dns.Fqdn(firstNonEmpty(spec.Zone, runtime.Name)))
		ttl := uint32(spec.TTL)
		if ttl == 0 {
			ttl = 300
		}
		zone := &zoneData{
			ResourceName: strings.TrimSpace(runtime.Name),
			Name:         name,
			TTL:          ttl,
			DHCP:         spec.DHCPDerived,
			DNSSEC:       spec.DNSSEC,
			Records:      map[string]zoneRecord{},
			PTR:          map[string]string{},
			ReverseZone:  map[string]bool{},
		}
		for _, reverse := range spec.ReverseZones {
			zone.ReverseZone[dns.Fqdn(reverse.Name)] = true
		}
		for _, record := range spec.Records {
			zone.addRecord(record.Hostname, record.IPv4, record.IPv6, uint32(record.TTL), zoneRecordSourceStatic)
		}
		zone.rebuildStaticPTR(spec.Records)
		if spec.DHCPDerived.LeaseFile != "" {
			if err := zone.loadDnsmasqLeases(spec.DHCPDerived.LeaseFile); err != nil {
				table.err = errors.Join(table.err, fmt.Errorf("DNS zone %q leases: %w", runtime.Name, err))
			}
		}
		for _, record := range runtime.DHCPHostRecords {
			zone.addRecoveredLease(record)
		}
		table.zones[name] = zone
	}
	return table
}

func (t *zoneTable) ZoneCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.zones)
}

func (t *zoneTable) CopyDynamicFrom(old *zoneTable) {
	if old == nil {
		return
	}
	type dynamicRecord struct {
		zoneName string
		record   zoneRecord
	}
	var records []dynamicRecord
	old.mu.RLock()
	for zoneName, zone := range old.zones {
		for _, record := range zone.Records {
			switch record.Source {
			case zoneRecordSourceActiveEvent:
				records = append(records, dynamicRecord{zoneName: zoneName, record: cloneZoneRecord(record)})
			case zoneRecordSourceStickySnapshot, zoneRecordSourceOldSticky:
				cloned := cloneZoneRecord(record)
				cloned.Source = zoneRecordSourceOldSticky
				records = append(records, dynamicRecord{zoneName: zoneName, record: cloned})
			}
		}
	}
	old.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, item := range records {
		zone := t.zones[item.zoneName]
		if zone == nil {
			continue
		}
		zone.mergeRecord(item.record)
	}
}

func cloneZoneRecord(record zoneRecord) zoneRecord {
	record.IPv4 = append([]string(nil), record.IPv4...)
	record.IPv6 = append([]string(nil), record.IPv6...)
	return record
}

func (t *zoneTable) Answer(req *dns.Msg, refs []string) (*dns.Msg, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(req.Question) == 0 {
		return nil, false
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET {
		return nil, false
	}
	selected := t.selectedZones(refs)
	sort.SliceStable(selected, func(i, j int) bool { return len(selected[i].Name) > len(selected[j].Name) })
	for _, zone := range selected {
		if msg, ok := zone.answer(req, q); ok {
			return msg, true
		}
	}
	return nil, false
}

func (t *zoneTable) selectedZones(refs []string) []*zoneData {
	if len(refs) == 0 {
		out := make([]*zoneData, 0, len(t.zones))
		for _, zone := range t.zones {
			out = append(out, zone)
		}
		return out
	}
	var out []*zoneData
	for _, ref := range refs {
		name := refName(ref)
		for _, zone := range t.zones {
			if zone.matchesRef(ref, name) {
				out = append(out, zone)
			}
		}
	}
	return out
}

func (z *zoneData) matchesRef(ref, name string) bool {
	zoneName := strings.TrimSuffix(z.Name, ".")
	refName := strings.TrimSuffix(name, ".")
	rawRef := strings.TrimSuffix(ref, ".")
	resourceName := strings.TrimSuffix(z.ResourceName, ".")
	return zoneName == refName || zoneName == rawRef || resourceName == refName || rawRef == "DNSZone/"+resourceName
}

func (t *zoneTable) ApplyLease(lease dhcpLeaseEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, zone := range t.zones {
		if len(zone.DHCP.Sources) == 0 {
			continue
		}
		switch lease.Action {
		case daemonapi.DHCPLeaseActionRemoved:
			zone.deleteDynamic(lease.IP)
		default:
			zone.addLeaseWithPriority(lease, zoneRecordSourceActiveEvent, zoneRecordPriorityLiveActiveEvent)
		}
	}
}

func (z *zoneData) answer(req *dns.Msg, q dns.Question) (*dns.Msg, bool) {
	name := dns.Fqdn(strings.ToLower(q.Name))
	if withinZone(name, z.Name) {
		msg := new(dns.Msg)
		msg.SetReply(req)
		msg.Authoritative = true
		record, ok := z.liveRecord(name)
		if !ok {
			// RFC 4592 style wildcard: with no exact match, walk up toward the
			// zone apex looking for a "*" record at the closest enclosing label.
			for rest := name; ; {
				idx := strings.IndexByte(rest, '.')
				if idx < 0 {
					break
				}
				rest = rest[idx+1:]
				if len(rest) < len(z.Name) {
					break
				}
				if wc, found := z.liveRecord("*." + rest); found {
					record = wc
					ok = true
					break
				}
				if rest == z.Name {
					break
				}
			}
		}
		switch q.Qtype {
		case dns.TypeA:
			for _, ip := range record.IPv4 {
				msg.Answer = append(msg.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: record.TTL}, A: net.ParseIP(ip)})
			}
		case dns.TypeAAAA:
			for _, ip := range record.IPv6 {
				msg.Answer = append(msg.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: record.TTL}, AAAA: net.ParseIP(ip)})
			}
		}
		if len(msg.Answer) > 0 {
			return msg, true
		}
		if !ok {
			// The name does not exist anywhere in this authoritative zone.
			msg.SetRcode(req, dns.RcodeNameError)
		}
		// Otherwise the name exists but has no record of the queried type
		// (NODATA): keep NOERROR with an empty answer so a dual-stack client
		// does not treat the whole name as nonexistent (e.g. an AAAA query for
		// an A-only record must not poison the A lookup via negative caching).
		return msg, true
	}
	if q.Qtype == dns.TypePTR && (strings.HasSuffix(name, ".in-addr.arpa.") || strings.HasSuffix(name, ".ip6.arpa.")) {
		target := z.PTR[name]
		record, active := z.liveRecord(strings.ToLower(dns.Fqdn(target)))
		if target == "" || !active {
			return nil, false
		}
		msg := new(dns.Msg)
		msg.SetReply(req)
		msg.Authoritative = true
		ttl := z.TTL
		if record.Source == zoneRecordSourceTypedLease && record.TTL < ttl {
			ttl = record.TTL
		}
		msg.Answer = append(msg.Answer, &dns.PTR{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl}, Ptr: target})
		return msg, true
	}
	return nil, false
}

func (z *zoneData) liveRecord(name string) (zoneRecord, bool) {
	r, ok := z.Records[name]
	if !ok || r.Source != zoneRecordSourceTypedLease {
		return r, ok
	}
	clock, err := boottime.Read()
	if err != nil || r.BootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= r.ExpiresNanos || !clock.WallTime.Before(r.ExpiresAt) {
		return zoneRecord{}, false
	}
	seconds := min(uint64(r.ExpiresAt.Sub(clock.WallTime)/time.Second), (r.ExpiresNanos-uint64(clock.BoottimeNanoseconds))/uint64(time.Second))
	if seconds < uint64(r.TTL) {
		r.TTL = uint32(seconds)
	}
	return r, true
}

func (z *zoneData) addRecord(hostname, ipv4, ipv6 string, ttl uint32, source zoneRecordSource) {
	z.addRecordWithPriority(hostname, ipv4, ipv6, ttl, source, source)
}

func (z *zoneData) addRecordWithPriority(hostname, ipv4, ipv6 string, ttl uint32, source, priority zoneRecordSource) {
	if hostname == "" {
		return
	}
	if ttl == 0 {
		ttl = z.TTL
	}
	record := zoneRecord{Hostname: z.recordName(hostname), TTL: ttl, Source: source}
	if ipv4 != "" {
		record.IPv4 = append(record.IPv4, ipv4)
	}
	if ipv6 != "" {
		record.IPv6 = append(record.IPv6, ipv6)
	}
	z.mergeRecordWithPriority(record, priority)
}

func (z *zoneData) recordName(hostname string) string {
	fqdn := strings.ToLower(dns.Fqdn(hostname))
	if !withinZone(fqdn, z.Name) {
		fqdn = dns.Fqdn(strings.TrimSuffix(hostname, ".") + "." + strings.TrimSuffix(z.Name, "."))
	}
	return fqdn
}

func (z *zoneData) rebuildStaticPTR(records []api.DNSZoneRecordSpec) {
	// Repeated hostnames retain their existing last-record-wins behavior.
	// Rebuild after all replacements so a surviving alias still has a PTR
	// when the original PTR owner was moved to another address.
	z.PTR = map[string]string{}
	for _, declared := range records {
		record := z.Records[strings.ToLower(z.recordName(declared.Hostname))]
		for _, ip := range []string{declared.IPv4, declared.IPv6} {
			if !contains(record.IPv4, ip) && !contains(record.IPv6, ip) {
				continue
			}
			if ptr, err := dns.ReverseAddr(ip); err == nil && z.PTR[dns.Fqdn(ptr)] == "" {
				z.PTR[dns.Fqdn(ptr)] = record.Hostname
			}
		}
	}
}

func (z *zoneData) addLease(lease dhcpLeaseEvent, source zoneRecordSource) {
	z.addLeaseWithPriority(lease, source, source)
}

func (z *zoneData) addLeaseWithPriority(lease dhcpLeaseEvent, source, priority zoneRecordSource) {
	if lease.IP == "" || lease.Hostname == "" {
		return
	}
	ttl := uint32(z.DHCP.TTL)
	if ttl == 0 {
		ttl = 60
	}
	ip := net.ParseIP(lease.IP)
	if ip == nil {
		return
	}
	if ip.To4() != nil {
		z.addRecordWithPriority(lease.Hostname, lease.IP, "", ttl, source, priority)
		return
	}
	z.addRecordWithPriority(lease.Hostname, "", lease.IP, ttl, source, priority)
}

func (z *zoneData) addRecoveredLease(record dnsresolver.RuntimeDHCPHostRecord) {
	if record.Hostname == "" || record.IP == "" {
		return
	}
	z.addLease(dhcpLeaseEvent{IP: record.IP, Hostname: record.Hostname}, zoneRecordSourceStickySnapshot)
}

func (z *zoneData) mergeRecord(record zoneRecord) {
	z.mergeRecordWithPriority(record, record.Source)
}

func (z *zoneData) mergeRecordWithPriority(record zoneRecord, priority zoneRecordSource) {
	if record.Hostname == "" {
		return
	}
	record.Hostname = dns.Fqdn(record.Hostname)
	name := strings.ToLower(record.Hostname)
	conflicts := map[string]zoneRecord{}
	sharedStaticPTR := map[string]bool{}
	if existing, exists := z.Records[name]; exists {
		if existing.Source > priority ||
			(existing.Source == priority && record.Source == zoneRecordSourceStickySnapshot) {
			return
		}
		conflicts[name] = existing
	}
	for _, ip := range append(append([]string(nil), record.IPv4...), record.IPv6...) {
		ptr, err := dns.ReverseAddr(ip)
		if err != nil {
			continue
		}
		target := z.PTR[dns.Fqdn(ptr)]
		if target == "" || strings.EqualFold(target, record.Hostname) {
			continue
		}
		existingName := strings.ToLower(dns.Fqdn(target))
		existing, exists := z.Records[existingName]
		if exists && existing.Source == zoneRecordSourceStatic && record.Source == zoneRecordSourceStatic {
			// Explicit static names may share an address. Keep both forward
			// records, with the first declared static name owning the PTR.
			sharedStaticPTR[dns.Fqdn(ptr)] = true
			continue
		}
		if !exists || existing.Source >= priority {
			return
		}
		conflicts[existingName] = existing
	}
	for conflictName := range conflicts {
		z.removeRecord(conflictName)
	}
	z.Records[name] = record
	for _, ip := range append(append([]string(nil), record.IPv4...), record.IPv6...) {
		if ptr, err := dns.ReverseAddr(ip); err == nil {
			if !sharedStaticPTR[dns.Fqdn(ptr)] {
				z.PTR[dns.Fqdn(ptr)] = record.Hostname
			}
		}
	}
}

func (z *zoneData) removeRecord(name string) {
	record, exists := z.Records[name]
	if !exists {
		return
	}
	delete(z.Records, name)
	for _, ip := range append(append([]string(nil), record.IPv4...), record.IPv6...) {
		ptr, err := dns.ReverseAddr(ip)
		if err != nil {
			continue
		}
		ptrName := dns.Fqdn(ptr)
		if strings.EqualFold(z.PTR[ptrName], record.Hostname) {
			delete(z.PTR, ptrName)
		}
	}
}

func (z *zoneData) deleteDynamic(ip string) {
	for name, record := range z.Records {
		if record.Source == zoneRecordSourceStatic {
			continue
		}
		if contains(record.IPv4, ip) || contains(record.IPv6, ip) {
			delete(z.Records, name)
		}
	}
	if ptr, err := dns.ReverseAddr(ip); err == nil {
		ptrName := dns.Fqdn(ptr)
		if target := z.PTR[ptrName]; target != "" {
			if record, exists := z.Records[strings.ToLower(dns.Fqdn(target))]; exists && record.Source == zoneRecordSourceStatic {
				return
			}
		}
		delete(z.PTR, ptrName)
	}
}

func withinZone(name, zone string) bool {
	return zone == "." || name == zone || strings.HasSuffix(name, "."+zone)
}

func (z *zoneData) loadDnsmasqLeases(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 64<<20 {
		return errors.New("lease input must be a regular file no larger than 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 64<<20 {
		return errors.New("lease input exceeds 64 MiB")
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] == "*" {
			continue
		}
		if !validHostname(fields[3]) || net.ParseIP(fields[2]) == nil {
			continue
		}
		z.addLease(dhcpLeaseEvent{MAC: fields[1], IP: fields[2], Hostname: fields[3]}, zoneRecordSourceActiveLeaseFile)
	}
	return nil
}

func sourceMatches(matches []string, qname string) bool {
	if len(matches) == 0 {
		return true
	}
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	for _, match := range matches {
		m := strings.TrimSuffix(strings.ToLower(match), ".")
		if m == "" || m == "." || name == m || strings.HasSuffix(name, "."+m) {
			return true
		}
	}
	return false
}

func dnsCacheKey(localAddr string, req *dns.Msg) string {
	// Class, CD/DO/RD flags and EDNS options (including ECS) all change what
	// an upstream may return. Only the transaction ID and name case are ignored.
	query := req.Copy()
	query.Id = 0
	for i := range query.Question {
		query.Question[i].Name = strings.ToLower(query.Question[i].Name)
	}
	wire, err := query.Pack()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(wire)
	return fmt.Sprintf("%s|%x", localAddr, sum)
}

func dnsMessageTTL(msg *dns.Msg, spec api.DNSResolverCacheSpec) time.Duration {
	if msg == nil || msg.Truncated || (msg.Rcode != dns.RcodeSuccess && msg.Rcode != dns.RcodeNameError) {
		return 0
	}
	ttl, found := uint32(60), false
	for _, section := range [][]dns.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, rr := range section {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			candidate := rr.Header().Ttl
			if len(msg.Answer) == 0 {
				if soa, ok := rr.(*dns.SOA); ok {
					candidate = min(candidate, soa.Minttl)
				}
			}
			if !found || candidate < ttl {
				ttl, found = candidate, true
			}
		}
	}
	out := time.Duration(ttl) * time.Second
	if len(msg.Answer) == 0 && spec.NegativeTTL != "" {
		if negative, err := time.ParseDuration(spec.NegativeTTL); err == nil {
			if !found || negative < out {
				out = negative
			}
		}
	}
	if spec.MinTTL != "" {
		if minimum, err := time.ParseDuration(spec.MinTTL); err == nil && out < minimum {
			out = minimum
		}
	}
	if spec.MaxTTL != "" {
		if maximum, err := time.ParseDuration(spec.MaxTTL); err == nil && out > maximum {
			out = maximum
		}
	}
	return out
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
