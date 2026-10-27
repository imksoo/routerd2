package runtimeplan

import (
	"encoding/json"
	"errors"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
)

// DNSInput returns only the sources authorized by this committed DNS instance.
// The result is complete: a missing, stopped or expired producer contributes no
// records. Client expiry remains original and DNS checks it when answering.
// Worker polling is separate from lease ACK and never holds up DHCP.
func (r *Runtime) DNSInput(moduleName string) (dnsconfig.Input, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[moduleName]
	if !r.configured || !ok || binding.instance.Role != "dns" {
		return dnsconfig.Input{}, errors.New("no committed DNS consumer")
	}
	cfg := r.config.protocols.dns[moduleName]
	out := dnsconfig.Input{Resource: cfg.Runtime.Resource, ConfigDigest: binding.digest, Sets: []dnsconfig.LeaseSet{}}
	clock, err := boottime.Read()
	if err != nil {
		return out, err
	}
	for _, zone := range cfg.Runtime.Zones {
		for _, ref := range zone.Spec.DHCPDerived.Sources {
			source, valid := dnsconfig.LeaseSourceName(ref)
			if !valid {
				return out, errors.New("invalid committed DNS lease source")
			}
			u, has := r.records["service."+source]
			if !has || u.Role != "dhcp_server" {
				continue
			}
			s := r.dhcpServers[u.Name]
			if !s.Active || !dhcpserverconfig.CurrentObservation(s, clock) {
				continue
			}
			set := dnsconfig.LeaseSet{Zone: zone.Name, Source: source, Epoch: s.Epoch, Sequence: s.Sequence, BootID: s.BootID, ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ExpiresNanos, ObservedAt: s.ObservedAt, Records: []dnsconfig.LeaseRecord{}}
			for _, lease := range s.Leases {
				if lease.Hostname != "" {
					set.Records = append(set.Records, dnsconfig.LeaseRecord{Hostname: lease.Hostname, Address: lease.IPAddress, ExpiresAt: lease.ExpiresAt})
				}
			}
			out.Sets = append(out.Sets, set)
		}
	}

	for _, source := range cfg.Runtime.Spec.Sources {
		for _, ref := range source.UpstreamFrom {
			name, err := dnsconfig.UpstreamProvider(ref)
			if err != nil {
				return out, err
			}
			update, found := r.records["service."+name]
			if !found || update.Role != "pppoe" {
				continue
			}
			var observed pppoeconfig.Snapshot
			if err := json.Unmarshal(update.Snapshot, &observed); err != nil {
				return out, err
			}
			if observed.Active && observed.ValidUntil.Live(clock) {
				out.Upstreams = append(out.Upstreams, dnsconfig.LearnedUpstream{Source: source.Name, Provider: name, Servers: observed.DNSServers, ValidUntil: observed.ValidUntil})
			}
		}
	}
	out = dnsconfig.NormalizeInput(out)
	return out, dnsconfig.ValidateInput(cfg.Runtime, out, clock)
}
