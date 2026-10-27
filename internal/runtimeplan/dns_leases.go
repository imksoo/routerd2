package runtimeplan

import (
	"encoding/json"
	"errors"
	"net/netip"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
)

type dnsPlan struct {
	runtime   dnsconfig.RuntimeConfig
	providers []dnsProvider
	targets   []dnsTarget
}

type dnsProvider struct {
	name, role string
}

type dnsTarget struct {
	provider  int
	source    string
	bootstrap bool
}

// Bind intent once. A provider shared by forwarding and bootstrap sources is
// decoded once per observation view, then delivered to its configured targets.
func prepareDNS(runtime dnsconfig.RuntimeConfig) (dnsPlan, error) {
	plan := dnsPlan{runtime: runtime}
	providers := map[string]int{}
	add := func(source string, ref api.StatusValueSourceSpec, bootstrap bool) error {
		name, role, err := dnsconfig.UpstreamProvider(ref)
		if err != nil {
			return err
		}
		index, found := providers[name]
		if !found {
			index = len(plan.providers)
			providers[name] = index
			plan.providers = append(plan.providers, dnsProvider{name: name, role: role})
		}
		target := dnsTarget{provider: index, source: source, bootstrap: bootstrap}
		for _, old := range plan.targets {
			if old == target {
				return nil
			}
		}
		plan.targets = append(plan.targets, target)
		return nil
	}
	for _, source := range runtime.Spec.Sources {
		for _, ref := range source.UpstreamFrom {
			if err := add(source.Name, ref, false); err != nil {
				return plan, err
			}
		}
		for _, ref := range source.BootstrapResolverFrom {
			if err := add(source.Name, ref, true); err != nil {
				return plan, err
			}
		}
	}
	return plan, nil
}

type dnsEndpoint struct {
	address netip.Addr
	until   lifetime.Deadline
}

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
	out := dnsconfig.Input{Resource: cfg.runtime.Resource, ConfigDigest: binding.digest, Sets: []dnsconfig.LeaseSet{}}
	clock, err := boottime.Read()
	if err != nil {
		return out, err
	}
	for _, zone := range cfg.runtime.Zones {
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

	learned := make([][]dnsEndpoint, len(cfg.providers))
	for index, provider := range cfg.providers {
		update, found := r.records["service."+provider.name]
		if !found || update.Role != provider.role {
			continue
		}
		add := func(address, iface string, until lifetime.Deadline) {
			if !until.Live(clock) {
				return
			}
			ip, err := netip.ParseAddr(address)
			if err != nil || ip.Is4In6() || ip.IsUnspecified() || ip.IsMulticast() {
				return
			}
			if ip.Is6() && ip.IsLinkLocalUnicast() {
				ip = ip.WithZone(iface)
			}
			for _, old := range learned[index] {
				if old.address == ip {
					return
				}
			}
			learned[index] = append(learned[index], dnsEndpoint{address: ip, until: until})
		}
		switch provider.role {
		case "pppoe":
			var observed pppoeconfig.Snapshot
			if err := json.Unmarshal(update.Snapshot, &observed); err != nil {
				return out, err
			}
			if observed.Active {
				for _, server := range observed.DNSServers {
					add(server, "", observed.ValidUntil)
				}
			}
		case "dhcp4":
			var observed dhcp4.Snapshot
			if err := json.Unmarshal(update.Snapshot, &observed); err != nil {
				return out, err
			}
			if observed.Active {
				until := lifetime.Deadline{BootID: observed.LeaseClock.BootID, Nanos: observed.LeaseClock.ExpiresNanos}
				for _, server := range observed.DNSServers {
					add(server, observed.Interface, until)
				}
			}
		case "dhcp6":
			var observed dhcp6config.Snapshot
			if err := json.Unmarshal(update.Snapshot, &observed); err != nil {
				return out, err
			}
			if until, current := observed.InformationUntil(clock); current {
				for _, server := range observed.DNSServers {
					add(server, observed.Interface, until)
				}
			}
		case "ra_client":
			var observed raclient.Snapshot
			if err := json.Unmarshal(update.Snapshot, &observed); err != nil {
				return out, err
			}
			if observed.Active && observed.BootID == clock.BootID {
				for _, server := range observed.DNSServers {
					add(server.Address, observed.Interface, server.ValidUntil)
				}
			}

		}
	}
	for _, target := range cfg.targets {
		for _, server := range learned[target.provider] {
			endpoint := dnsconfig.LearnedUpstream{Source: target.source, Provider: cfg.providers[target.provider].name, Address: server.address, ValidUntil: server.until}
			if target.bootstrap {
				out.Bootstraps = append(out.Bootstraps, endpoint)
			} else {
				out.Upstreams = append(out.Upstreams, endpoint)
			}
		}
	}
	return out, nil
}
