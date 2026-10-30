// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Ingress uses ordinary Health providers and ordered, conditional NAT rules.
// The kernel's first matching DNAT rule selects the first healthy backend;
// an absent or expired earlier probe therefore cannot obstruct a later one.
func (c *compiler) ingressServices(vrrpNames map[string]string) []resourceprogram.Resource {
	var nodes []resourceprogram.Resource
	groups := map[string][]string{}
	ages := map[string]int{}
	for _, r := range c.resourcesOfKind("IngressService") {
		c.allow(r, "listen", "backends", "hairpin", "healthCheck", "policy", "when")
		spec := r.Spec.(api.IngressServiceSpec)
		if spec.Policy.Selection != "" && spec.Policy.Selection != "failover" {
			c.reject(r, "policy.selection", "unsupported_semantics", "ingress migration supports ordered failover")
			continue
		}
		if spec.HealthCheck.ExpectedBody != "" {
			c.reject(r, "healthCheck.expectedBody", "unsupported_semantics", "response body matching is not implemented by the scoped Health probe")
		}
		if spec.Listen.AddressFrom.Resource != "" {
			c.reject(r, "listen.addressFrom", "unsupported_semantics", "ingress requires the declared literal VIP, not a captured status address")
		}
		listener, err := netip.ParseAddr(spec.Listen.Address)
		if err != nil || !listener.Is4() || listener.IsUnspecified() || listener.IsMulticast() {
			c.reject(r, "listen.address", "invalid_value", "ingress needs a literal IPv4 listen address")
			continue
		}
		iface := c.interfaceName(r, "listen.interface", spec.Listen.Interface, false)
		c.ensureInterface(iface)
		inputs := []string{iface}
		// Native elections publish VIPs on their VMAC rather than the physical LAN.
		for _, vip := range c.resourcesOfKind("VirtualAddress") {
			v := vip.Spec.(api.VirtualAddressSpec)
			address := strings.Split(v.Address, "/")[0]
			if address == listener.String() && v.Mode == "vrrp" {
				if policy, ok := c.options.VRRP["VirtualAddress/"+vip.Metadata.Name]; ok && policy.VIPInterface != "" {
					inputs = append(inputs, policy.VIPInterface)
				}
			}
		}
		if spec.Listen.Protocol != "tcp" && spec.Listen.Protocol != "udp" {
			c.reject(r, "listen.protocol", "invalid_value", "ingress transport must be tcp or udp")
		}
		enabled := true
		if c.document.Network.Forwarding.IPv4 != nil && !*c.document.Network.Forwarding.IPv4 {
			c.reject(r, "listen", "conflict", "ingress forwarding conflicts with disabled IPv4 forwarding")
		} else {
			c.document.Network.Forwarding.IPv4 = &enabled
		}
		firewall := c.document.Network.Firewall
		if firewall == nil {
			firewall = &network.Firewall{DisableForward: true}
			c.document.Network.Firewall = firewall
		}
		if firewall.ForwardChain != nil || len(firewall.Forward) != 0 {
			c.reject(r, "listen", "unsupported_semantics", "ingress forwarding exceptions in an existing filtering policy need explicit native rules")
		}
		if firewall.Input == nil {
			firewall.Input = &network.FirewallChain{Policy: "accept"}
		}
		reject := first(spec.Policy.OnNoHealthyBackends, "drop")
		for _, input := range inputs {
			// A healthy DNAT has already changed the destination and follows FORWARD.
			// Only an untranslated request to the unavailable local VIP reaches INPUT.
			firewall.Input.Rules = append([]network.FirewallRule{{Match: network.PacketMatch{Family: 4, InInterface: input, Destination: listener.String() + "/32", Protocol: spec.Listen.Protocol, DestinationPorts: []network.PortRange{{Start: spec.Listen.Port}}}, Action: reject}}, firewall.Input.Rules...)
		}
		var members []string
		when := c.when(r, spec.When, vrrpNames)
		for i, backend := range spec.Backends {
			if backend.AddressFrom.Resource != "" || backend.Weight != 0 {
				c.reject(r, "backends", "unsupported_semantics", "ordered ingress requires literal backends without distribution weights")
			}
			address, err := netip.ParseAddr(backend.Address)
			if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
				c.reject(r, "backends.address", "invalid_value", "ingress backend must be literal IPv4")
				continue
			}
			name := c.serviceName(r, fmt.Sprintf("ing%d", i+1))
			cfg := health.Config{Resource: name, Interface: iface, Target: address.String(), Protocol: first(spec.HealthCheck.Protocol, "tcp"), Port: backend.Port,
				IntervalMillis:   c.healthDuration(r, "healthCheck.interval", spec.HealthCheck.Interval, 5*time.Second),
				TimeoutMillis:    c.healthDuration(r, "healthCheck.timeout", spec.HealthCheck.Timeout, time.Second),
				HealthyThreshold: max(1, spec.HealthCheck.HealthyThreshold), UnhealthyThreshold: max(1, spec.HealthCheck.UnhealthyThreshold)}
			if cfg.Protocol == "http" || cfg.Protocol == "https" {
				cfg.HTTP = &health.HTTPCheck{Path: spec.HealthCheck.Path, Host: spec.HealthCheck.Host, ExpectedStatus: spec.HealthCheck.ExpectedStatus, TLSSkipVerify: spec.HealthCheck.TLSSkipVerify}
			}
			service := serviceconfig.Service{Name: name, Health: &cfg}
			id := "service/" + name
			nodes = append(nodes, resourceprogram.Resource{ID: id, Service: &service, When: when})
			c.target(r, id)
			members = append(members, id)
			ages[r.Kind+"/"+r.Metadata.Name] = int(cfg.Freshness() / time.Millisecond)
			condition := resourceprogram.FactCondition{Fact: service.ModuleName() + ".active"}
			if when != nil {
				condition = resourceprogram.FactCondition{All: []resourceprogram.FactCondition{*when, condition}}
			}
			var rules []network.NATRule
			for _, input := range inputs {
				rules = append(rules, network.NATRule{Type: "dnat", Match: network.PacketMatch{Family: 4, InInterface: input, Destination: listener.String() + "/32", Protocol: spec.Listen.Protocol, DestinationPorts: []network.PortRange{{Start: spec.Listen.Port}}}, Address: address.String(), Ports: &network.PortRange{Start: backend.Port}})
			}
			hairpin := []string{}
			if spec.Hairpin.Mode != "off" {
				for _, ref := range spec.Hairpin.Interfaces {
					hairpin = append(hairpin, c.interfaceName(r, "hairpin.interfaces", ref, false))
				}
				if spec.Hairpin.Mode == "auto" || spec.Hairpin.Mode == "" {
					// This is the original renderer's same-private-/24 fallback. Explicit
					// interfaces remain authoritative when the LAN address is learned.
					if listener.IsPrivate() && address.IsPrivate() && netip.PrefixFrom(listener, 24).Contains(address) && !slices.Contains(hairpin, iface) {
						hairpin = append(hairpin, iface)
					}
				}
			}
			for _, output := range hairpin {
				for _, input := range inputs {
					rules = append(rules, network.NATRule{Type: "masquerade", Match: network.PacketMatch{Family: 4, InInterface: input, OutInterface: output, Destination: address.String() + "/32", Protocol: spec.Listen.Protocol, DestinationPorts: []network.PortRange{{Start: backend.Port}}}, OriginalDestination: listener.String(), OriginalPort: spec.Listen.Port})
				}
			}
			nodes = append(nodes, resourceprogram.Resource{ID: fmt.Sprintf("ingress/%s/%04d", r.Metadata.Name, i), NAT: rules, When: &condition})
		}
		if len(members) == 0 {
			c.reject(r, "backends", "required", "ingress requires at least one backend")
		}
		groups[r.Kind+"/"+r.Metadata.Name] = members
		c.note(r, "Preserves ordered healthy-backend DNAT, explicit HTTP/TLS checks and same-LAN hairpin restricted to the original VIP/port. Health thresholds and finite observations feed existing conditional network projection; no extra monitoring daemon is added.")
	}
	for _, r := range c.resourcesOfKind("VirtualAddress") {
		spec := r.Spec.(api.VirtualAddressSpec)
		if len(spec.Track) == 0 {
			continue
		}
		c.note(r, "Ingress tracking retains the configured penalty and confirmation counts. Each confirmation consumes a distinct round of backend Health observations at the declared probe interval; repeated controller polls do not add confirmations. Any unhealthy backend lowers priority while forwarding can use the remaining healthy backends.")
		for _, track := range spec.Track {
			if source := c.byRef[track.Resource]; source != nil && meaningful(child(source.node, "when")) {
				c.reject(r, "track", "unsupported_semantics", "conditional ingress tracking needs its original neutral-state behavior; it cannot be replaced by missing Health observations")
				continue
			}
			members := groups[track.Resource]
			if len(members) == 0 {
				c.reject(r, "track", "unsupported_semantics", "tracking needs a supported complete ingress health group")
				continue
			}
			for i := range c.document.Services {
				service := &c.document.Services[i]
				if service.VRRP != nil && service.Name == vrrpNames["VirtualAddress/"+r.Metadata.Name] {
					service.VRRP.Track = append(service.VRRP.Track, vrrp.Track{Resource: track.Resource, IngressHealth: members, Penalty: track.UnhealthyPenalty, HealthyThreshold: track.ConfirmConsecutiveHealthy, UnhealthyThreshold: track.ConfirmConsecutiveUnhealthy, MaxAgeMillis: ages[track.Resource]})
				}
			}
		}
	}
	return nodes
}
