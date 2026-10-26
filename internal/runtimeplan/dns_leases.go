package runtimeplan

import (
	"errors"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

// DNSLeases returns only the sources authorized by this committed DNS instance.
// The result is complete: a missing, stopped or expired producer contributes no
// records. Client expiry remains original and DNS checks it when answering.
// Worker polling is separate from lease ACK and never holds up DHCP.
func (r *Runtime) DNSLeases(moduleName string) (dnsconfig.LeaseProjection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.bindings[moduleName]
	if !r.configured || !ok || binding.instance.Role != "dns" || !binding.instance.DynamicDNS {
		return dnsconfig.LeaseProjection{}, errors.New("no committed native DNS lease consumer")
	}
	var cfg dnsconfig.Config
	if err := strict(binding.instance.Config, &cfg); err != nil {
		return dnsconfig.LeaseProjection{}, err
	}
	out := dnsconfig.LeaseProjection{Resource: cfg.Runtime.Resource, ConfigDigest: binding.digest, Sets: []dnsconfig.LeaseSet{}}
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
	out = dnsconfig.NormalizeLeaseProjection(out)
	return out, dnsconfig.ValidateLeaseProjection(cfg.Runtime, out, clock)
}
