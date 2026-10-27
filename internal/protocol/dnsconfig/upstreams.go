// SPDX-License-Identifier: BSD-3-Clause

package dnsconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
)

// UpstreamProvider narrows the operator's reference to the protocol fact this
// resolver consumes. It is not a generic status-field query or JSON expression.
func UpstreamProvider(ref api.StatusValueSourceSpec) (string, error) {
	name, found := strings.CutPrefix(ref.Resource, "PPPoESession/")
	if !found || name == "" || strings.Contains(name, "/") || ref.Field != "" && ref.Field != "dnsServers" {
		return "", errors.New("DNS upstreamFrom requires PPPoESession/name and dnsServers")
	}
	return name, nil
}

func validateUpstreams(c RuntimeConfig, p Input, clock boottime.Clock) error {
	allowed := map[string]bool{}
	for _, source := range c.Spec.Sources {
		for _, ref := range source.UpstreamFrom {
			provider, err := UpstreamProvider(ref)
			if err != nil {
				return err
			}
			allowed[source.Name+"/"+provider] = true
		}
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, source := range c.Spec.Sources {
		counts[source.Name] = len(source.Upstreams)
	}
	if len(p.Upstreams) > 256 {
		return errors.New("DNS input exceeds 256 upstream sets")
	}
	for _, set := range p.Upstreams {
		key := set.Source + "/" + set.Provider
		if !allowed[key] || seen[key] || len(set.Servers) > 64 {
			return fmt.Errorf("DNS upstream source %s is unauthorized, duplicate or oversized", key)
		}
		seen[key] = true
		counts[set.Source] += len(set.Servers)
		if counts[set.Source] > 64 {
			return errors.New("DNS source exceeds 64 static and learned upstreams")
		}
		if err := set.ValidUntil.Validate(); err != nil {
			return err
		}
		if set.ValidUntil.BootID != clock.BootID || set.ValidUntil.Nanos == 0 {
			return errors.New("learned DNS requires its finite original boot deadline")
		}
		for _, server := range set.Servers {
			address, err := netip.ParseAddr(server)
			if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
				return errors.New("PPP DNS input requires unicast IPv4 server addresses")
			}
		}
	}
	return nil
}

func NeedsInput(c RuntimeConfig) bool {
	for _, source := range c.Spec.Sources {
		if len(source.UpstreamFrom) != 0 {
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
