// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"reflect"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type leaseSyncInput struct {
	source, command, interval string
	targets                   []api.LeaseSyncTargetSpec
	when                      api.ResourceWhenSpec
}

func leaseSyncKind(kind string) bool {
	switch kind {
	case "DHCPv4ServerLeaseSync", "DHCPv6PrefixDelegationLeaseSync", "DHCPv6ServerLeaseSync":
		return true
	}
	return false
}

func leaseSyncSpec(r resource) leaseSyncInput {
	switch s := r.Spec.(type) {
	case api.DHCPv4ServerLeaseSyncSpec:
		return leaseSyncInput{s.Source.Resource, s.Command, s.Interval, s.Targets, s.When}
	case api.DHCPv6PrefixDelegationLeaseSyncSpec:
		return leaseSyncInput{s.Source.Resource, s.Command, s.Interval, s.Targets, s.When}
	case api.DHCPv6ServerLeaseSyncSpec:
		return leaseSyncInput{s.Source.Resource, s.Command, s.Interval, s.Targets, s.When}
	}
	return leaseSyncInput{}
}

func (c *compiler) leaseService(r resource, role string) *serviceconfig.Service {
	name := c.serviceName(r, role)
	for i := range c.document.Services {
		if c.document.Services[i].Name == name {
			return &c.document.Services[i]
		}
	}
	return nil
}

func (c *compiler) leaseSync(vrrpNames map[string]string) {
	for _, r := range c.resources {
		if !leaseSyncKind(r.Kind) {
			continue
		}
		c.allow(r, "source", "command", "interval", "targets")
		input := leaseSyncSpec(r)
		if input.command != "" && input.command != "rsync" {
			c.reject(r, "command", "unsupported_semantics", "a custom synchronization executable is not replaced by a native lease transfer")
		}
		if input.interval != "" {
			if interval, err := time.ParseDuration(input.interval); err != nil || interval <= 0 {
				c.reject(r, "interval", "invalid_value", "legacy synchronization interval must be positive")
			}
		}
		if len(input.targets) != 1 {
			c.reject(r, "targets", "unsupported_semantics", "native lease synchronization requires one explicitly authenticated peer")
		}
		policy, exists := c.options.LeaseSync[r.Kind+"/"+r.Metadata.Name]
		if !exists {
			c.reject(r, "", "explicit_policy_required", "lease_sync policy must explicitly replace SSH credentials, endpoints and periodic file-copy semantics")
			continue
		}
		c.usePolicy(r, "lease_sync")
		if r.Kind == "DHCPv6ServerLeaseSync" {
			continue // Resolve its shared DHCPv4 file owner after all owners exist.
		}
		if policy.SharedWith != "" || policy.Replication == nil {
			c.policyIssue(r, "lease_sync", "replication", "policy_required", "select native peer endpoints and certificate file paths; shared_with applies only to a stateless DHCPv6 server")
			continue
		}
		if err := peertls.Validate(*policy.Replication); err != nil {
			c.policyIssue(r, "lease_sync", "replication", "policy_validation", "native replication requires distinct unicast endpoints and absolute credential file paths")
			continue
		}
		when := c.when(r, input.when, vrrpNames)
		var owner *serviceconfig.Service
		for i := range c.document.Services {
			candidate := &c.document.Services[i]
			if candidate.VRRP != nil && reflect.DeepEqual(when, &resourceprogram.FactCondition{Fact: candidate.ModuleName() + ".elected"}) {
				owner = candidate
				break
			}
		}
		if owner == nil {
			c.reject(r, "when", "unsupported_semantics", "lease synchronization must follow one compiled VRRP master condition")
			continue
		}
		kind, role := "DHCPv4Server", "dhcp"
		if r.Kind == "DHCPv6PrefixDelegationLeaseSync" {
			kind, role = "DHCPv6PrefixDelegation", "pd"
		}
		source := c.getRef(r, "source.resource", input.source, kind)
		if source == nil {
			continue
		}
		service := c.leaseService(*source, role)
		if service == nil {
			continue // Its compilation diagnostic already prevents publication.
		}
		var sourceWhen api.ResourceWhenSpec
		if service.DHCP6 != nil {
			sourceWhen = source.Spec.(api.DHCPv6PrefixDelegationSpec).When
		} else {
			sourceWhen = source.Spec.(api.DHCPv4ServerSpec).When
		}
		if !reflect.DeepEqual(c.when(*source, sourceWhen, vrrpNames), when) {
			c.reject(r, "source.resource", "conflict", "the source and synchronization must have the same VRRP master condition")
			continue
		}
		peer := *policy.Replication
		if service.DHCP6 != nil {
			if service.DHCP6.HA != nil {
				c.policyIssue(r, "lease_sync", "replication", "conflict", "PD has one synchronization owner and obtains its initial lease from the upstream server")
				continue
			}
			if service.DHCP6.ClientDUID == "" {
				c.reject(r, "source.resource", "required", "replicated PD requires an explicit shared client DUID in the source")
				continue
			}
			service.DHCP6.HA = &serviceconfig.LeaseHA{Owner: owner.Name, Replication: peer}
		} else {
			if service.DHCPServer.HA != nil {
				c.policyIssue(r, "lease_sync", "replication", "conflict", "each server has one synchronization owner")
				continue
			}
			vip := owner.VRRPConfig()
			address, err := netip.ParsePrefix(vip.Addresses[0])
			prefix, _ := netip.ParsePrefix(service.DHCPServer.Primary.Prefix)
			if vip.Family != "ipv4" || vip.Interface != service.DHCPServer.Interface || err != nil || !prefix.Contains(address.Addr()) {
				c.reject(r, "source.resource", "conflict", "the DHCP server must share its subnet and physical LAN with its IPv4 VRRP owner")
				continue
			}
			service.DHCPServer.Interface = vip.VIPInterface
			service.DHCPServer.ServerAddress = address.Addr().String()
			service.DHCPServer.HA = &serviceconfig.DHCPServerHA{Owner: owner.Name, Replication: peer}
		}
		c.target(r, "services/"+service.Name+"/ha")
		c.note(r, "Explicit lease_sync policy replaces rsync/SSH identity, destination and interval with the native mutually authenticated two-node protocol. No old keys or lease files are read or imported. The VRRP owner selects serving versus standby synchronization; original lease deadlines and DHCPv4 sticky history belong to the existing protocol engine.")
		c.note(*source, "The declared master condition is enforced by ha.owner; the standby worker remains running to synchronize state.")
	}
	for _, r := range c.resources {
		if r.Kind == "DHCPv6ServerLeaseSync" {
			c.sharedStatelessLeaseSync(r, vrrpNames)
		}
		if r.Kind != "DHCPv4Server" && r.Kind != "DHCPv4Reservation" {
			continue
		}
		if !meaningful(child(r.node, "when")) {
			continue
		}
		source := &r
		var when api.ResourceWhenSpec
		if r.Kind == "DHCPv4Reservation" {
			reservation := r.Spec.(api.DHCPv4ReservationSpec)
			when = reservation.When
			if reservation.Server != "" {
				source = c.getRef(r, "server", reservation.Server, "DHCPv4Server")
			} else {
				source = nil
				for i := range c.resources {
					if c.resources[i].Kind == "DHCPv4Server" {
						source = &c.resources[i]
						break
					}
				}
				// dhcpServers already rejects an unqualified reservation with multiple servers.
			}
		} else {
			when = r.Spec.(api.DHCPv4ServerSpec).When
		}
		if source == nil || !meaningful(child(r.node, "when")) {
			continue
		}
		server := c.leaseService(*source, "dhcp")
		if server == nil || server.DHCPServer.HA == nil || !reflect.DeepEqual(c.when(r, when, vrrpNames), &resourceprogram.FactCondition{Fact: "service." + server.DHCPServer.HA.Owner + ".elected"}) {
			c.reject(r, "when", "unsupported_semantics", "conditional DHCP intent requires the same explicitly migrated lease owner's master condition")
		}
	}
}

func (c *compiler) sharedStatelessLeaseSync(r resource, vrrpNames map[string]string) {
	policy, exists := c.options.LeaseSync[r.Kind+"/"+r.Metadata.Name]
	if !exists {
		return
	}
	if policy.SharedWith == "" || policy.Replication != nil {
		c.policyIssue(r, "lease_sync", "shared_with", "policy_required", "a stateless server has no IPv6 bindings; explicitly identify the DHCPv4 sync owning its formerly shared dnsmasq lease file")
		return
	}
	input := leaseSyncSpec(r)
	source := c.getRef(r, "source.resource", input.source, "DHCPv6Server")
	shared := c.byRef[policy.SharedWith]
	if source == nil || shared == nil || shared.Kind != "DHCPv4ServerLeaseSync" {
		c.policyIssue(r, "lease_sync", "shared_with", "policy_reference", "shared_with must identify an existing DHCPv4ServerLeaseSync by exact Kind/name")
		return
	}
	spec := source.Spec.(api.DHCPv6ServerSpec)
	original := leaseSyncSpec(*shared)
	server := c.getRef(*shared, "source.resource", original.source, "DHCPv4Server")
	if server == nil {
		return
	}
	service := c.leaseService(*server, "dhcp")
	if spec.Mode != "stateless" || spec.LeaseFile != "" || server.Spec.(api.DHCPv4ServerSpec).LeaseFile != "" || service == nil || service.DHCPServer.HA == nil || !reflect.DeepEqual(input.targets, original.targets) || !reflect.DeepEqual(c.when(r, input.when, vrrpNames), c.when(*shared, original.when, vrrpNames)) {
		c.reject(r, "source.resource", "unsupported_semantics", "only the stateless server's same default dnsmasq file, target and owner can share the migrated DHCPv4 synchronization")
		return
	}
	c.target(r, "services/"+service.Name+"/ha")
	c.note(r, "Explicit shared_with policy retains synchronization of the old common dnsmasq file through its DHCPv4 owner. The stateless DHCPv6 server allocates no address leases; no second replication engine or file copier is created.")
}
