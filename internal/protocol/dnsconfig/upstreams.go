// SPDX-License-Identifier: BSD-3-Clause

package dnsconfig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
)

// UpstreamProvider narrows the operator's reference to the protocol fact this
// resolver consumes. It is not a generic status-field query or JSON expression.
func UpstreamProvider(ref api.StatusValueSourceSpec) (name, role string, err error) {
	kind, name, found := strings.Cut(ref.Resource, "/")
	if !found || name == "" || strings.Contains(name, "/") || ref.Field != "" && ref.Field != "dnsServers" {
		return "", "", errors.New("DNS upstreamFrom requires Kind/name and dnsServers")
	}
	switch kind {
	case "PPPoESession":
		role = "pppoe"
	case "DHCPv4Client":
		role = "dhcp4"
	case "DHCPv6Address", "DHCPv6PrefixDelegation", "DHCPv6Information":
		role = "dhcp6"
	case "IPv6RAAddress":
		role = "ra_client"
	case "TailscaleNode":
		role = "tailscale"
	default:
		return "", "", fmt.Errorf("unsupported DNS upstream provider %s", kind)
	}
	return name, role, nil
}

func validateUpstreams(c RuntimeConfig, p Input, clock boottime.Clock) error {
	allowed := map[string]bool{}
	bootstrap := map[string]bool{}
	upstreamCounts, bootstrapCounts := map[string]int{}, map[string]int{}
	for _, source := range c.Spec.Sources {
		upstreamCounts[source.Name] = len(source.Upstreams)
		bootstrapCounts[source.Name] = len(source.BootstrapResolver)
		for _, ref := range source.UpstreamFrom {
			provider, _, err := UpstreamProvider(ref)
			if err != nil {
				return err
			}
			allowed[source.Name+"/"+provider] = true
		}
	}
	for _, source := range c.Spec.Sources {
		for _, ref := range source.BootstrapResolverFrom {
			provider, _, err := UpstreamProvider(ref)
			if err != nil {
				return err
			}
			bootstrap[source.Name+"/"+provider] = true
		}
	}
	if err := validateLearnedServers(bootstrap, bootstrapCounts, p.Bootstraps, clock); err != nil {
		return err
	}
	return validateLearnedServers(allowed, upstreamCounts, p.Upstreams, clock)
}

func validateLearnedServers(allowed map[string]bool, counts map[string]int, servers []LearnedUpstream, clock boottime.Clock) error {
	seen := map[string]bool{}
	if len(servers) > 16384 {
		return errors.New("DNS input exceeds 16384 learned endpoints")
	}
	for _, set := range servers {
		key := set.Source + "/" + set.Provider
		endpoint := key + "/" + set.Address.String()
		if !allowed[key] || seen[endpoint] {
			return fmt.Errorf("DNS upstream source %s is unauthorized or duplicate", key)
		}
		seen[endpoint] = true
		counts[set.Source]++
		if counts[set.Source] > 64 {
			return errors.New("DNS source exceeds 64 static and learned servers")
		}
		if err := set.ValidUntil.Validate(); err != nil {
			return err
		}
		if set.ValidUntil.BootID != clock.BootID {
			return errors.New("learned DNS requires its original boot deadline")
		}
		address := set.Address
		if !address.IsValid() || address.Is4In6() || address.IsUnspecified() || address.IsMulticast() || address.Is6() && address.IsLinkLocalUnicast() && address.Zone() == "" {
			return errors.New("learned DNS requires unicast addresses and scoped IPv6 link-local servers")
		}
	}
	return nil
}

func NeedsInput(c RuntimeConfig) bool {
	for _, source := range c.Spec.Sources {
		if len(source.UpstreamFrom) != 0 || len(source.BootstrapResolverFrom) != 0 {
			return true
		}
	}
	for _, zone := range c.Zones {
		if len(zone.Spec.DHCPDerived.Sources) != 0 {
			return true
		}
	}
	return false
}
