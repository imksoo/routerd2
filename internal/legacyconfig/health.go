// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) healthCheck(r resource) string {
	c.allow(r, "enabled", "type", "role", "addressFamily", "target", "targetSource", "interface", "protocol", "port", "interval", "timeout", "healthyThreshold", "unhealthyThreshold")
	s := old.ResolveSpecForResource(&c.legacy, r.Metadata.Name, r.Spec.(api.HealthCheckSpec))
	policy, explicit := c.options.HealthChecks[r.Kind+"/"+r.Metadata.Name]
	if explicit {
		c.usePolicy(r, "health_checks")
	}
	if s.Enabled != nil && !*s.Enabled {
		c.note(r, "Disabled check creates no native probe or healthy observation. Re-enabling requires declaring the native scoped service.")
		return ""
	}
	if s.Type != "" && s.Type != "ping" {
		c.reject(r, "type", "unsupported_semantics", "only the original ping type with an explicit supported protocol can be imported")
	}
	if s.TargetSource != "" && s.TargetSource != "auto" && s.TargetSource != "static" {
		c.reject(r, "targetSource", "unsupported_semantics", "dynamic gateway or DS-Lite target discovery needs a live producer binding; a snapshot cannot become static probe intent")
	}
	target, err := netip.ParseAddr(s.Target)
	if err != nil || target.IsUnspecified() || target.IsMulticast() || target.Is4In6() || target.Zone() != "" {
		c.reject(r, "target", "unsupported_semantics", "a unicast IPv4/IPv6 literal target is required; automatic or hostname targets need a native live binding")
		return ""
	}
	if s.AddressFamily != "" && s.AddressFamily != "ipv4" && s.AddressFamily != "ipv6" || s.AddressFamily == "ipv4" && !target.Is4() || s.AddressFamily == "ipv6" && !target.Is6() {
		c.reject(r, "addressFamily", "invalid_value", "addressFamily must agree with the literal probe target")
	}
	switch s.Role {
	case "", "link", "next-hop", "internet", "service", "policy":
	default:
		c.reject(r, "role", "invalid_value", "unknown health classification")
	}
	name := ""
	if source := first(s.SourceInterface, s.Interface); source != "" {
		name = c.interfaceName(r, "interface", source, true)
	}
	if !explicit {
		if s.FwMark < 0 || uint64(s.FwMark) > 1<<32-1 {
			c.reject(r, "interface", "invalid_value", "derived probe mark must fit uint32")
			return ""
		}
		policy.Mark, policy.SourceAddress = uint32(s.FwMark), s.SourceAddress
	}
	if explicit {
		selected := c.interfaceName(r, "interface", policy.Interface, false)
		if name != "" && name != selected {
			c.policyIssue(r, "health_checks", "interface", "conflict", "probe scope must retain the declared legacy interface")
		}
		name = selected
	}
	if name == "" {
		c.reject(r, "interface", "explicit_policy_required", "health_checks policy must select the probe's interface instead of following an unscoped default route")
		return ""
	}
	if _, dynamic := c.dynamicTunnels[name]; !dynamic {
		c.ensureInterface(name)
	}
	interval := c.healthDuration(r, "interval", s.Interval, 30*time.Second)
	timeout := c.healthDuration(r, "timeout", s.Timeout, 3*time.Second)
	service := c.serviceName(r, "health")
	c.addService(r, serviceconfig.Service{Name: service, Health: &health.Config{
		Resource: service, Interface: name, SourceAddress: policy.SourceAddress, Mark: policy.Mark,
		Target: target.String(), Protocol: s.Protocol, Port: s.Port,
		IntervalMillis: interval, TimeoutMillis: timeout,
		HealthyThreshold: s.HealthyThreshold, UnhealthyThreshold: s.UnhealthyThreshold,
	}})
	c.note(r, "The native worker uses routerd's probe and threshold engine. Empty intervals/timeouts retain routerd's 30s/3s defaults; thresholds retain 1/3. Probe interface and mark are derived by routerd ResolveSpecForResource from declared routing references, or selected by explicit migration policy. Legacy role is classification, not activation policy. VRRP master/backup conditions use finite election facts; unsupported conditions and live target discovery are not silently dropped.")
	return service
}

func (c *compiler) healthDuration(r resource, field, value string, fallback time.Duration) int {
	d := fallback
	if value != "" {
		var err error
		d, err = time.ParseDuration(value)
		if err != nil || d <= 0 || d%time.Millisecond != 0 {
			c.reject(r, field, "unsupported_semantics", "probe duration must be positive and representable in whole native milliseconds")
			return 0
		}
	}
	return int(d / time.Millisecond)
}
