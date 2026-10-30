// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
	"github.com/imksoo/routerd2/internal/retention"
)

func (c *compiler) logRetention(r resource) {
	c.allow(r, "retention", "signals", "sinks", "vacuum", "schedule")
	spec := r.Spec.(api.LogRetentionSpec)
	if c.retention != nil {
		c.reject(r, "", "unsupported_semantics", "native event history has one retention owner; combine policies explicitly")
		return
	}
	value := strings.TrimSpace(spec.Retention)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days < 1 || days > 106751 {
			c.reject(r, "retention", "invalid_value", "invalid positive retention duration")
			return
		}
		value = strconv.FormatInt(days*24, 10) + "h"
	}
	age, err := time.ParseDuration(value)
	if err != nil || age < time.Second {
		c.reject(r, "retention", "invalid_value", "retention must be at least one second")
		return
	}
	if spec.Schedule != "" && spec.Schedule != "daily" {
		c.reject(r, "schedule", "unsupported_semantics", "only daily legacy retention is defined")
		return
	}
	signals := spec.Signals
	if len(signals) == 0 {
		signals = []string{"events", "accessLogs", "pluginRuns", "dnsQueries", "trafficFlows", "firewallEvents", "dhcpFingerprints"}
	}
	events := false
	for _, signal := range signals {
		if signal == "events" {
			events = true
			continue
		}
		switch signal {
		case "accessLogs", "pluginRuns", "dnsQueries", "trafficFlows", "firewallEvents", "dhcpFingerprints":
			c.reject(r, "signals", "unsupported_semantics", "operational history dataset "+signal+" has no native retention owner")
		default:
			c.reject(r, "signals", "unsupported_semantics", "unknown operational history dataset")
			return
		}
	}
	if !events {
		c.reject(r, "signals", "unsupported_semantics", "requested datasets have no native history owner; event retention is not substituted")
		return
	}
	policy := retention.Config{MaxAgeSeconds: int64(age / time.Second), IntervalSeconds: 86400, Vacuum: spec.Vacuum}
	for _, ref := range spec.Sinks {
		target := c.getRef(r, "sinks", ref, "LogSink")
		if target == nil {
			continue
		}
		sink := target.Spec.(api.LogSinkSpec)
		if sink.Enabled != nil && !*sink.Enabled {
			continue
		}
		if sink.Type != "syslog" || sink.Syslog.Network != "udp" {
			c.reject(r, "sinks", "unsupported_semantics", "deletion-time archive currently supports UDP syslog only")
			continue
		}
		tag := sink.Syslog.Tag
		if tag == "" {
			tag = "routerd"
		}
		destination := syslogconfig.Normalize(syslogconfig.Config{Address: sink.Syslog.Address, Facility: sink.Syslog.Facility, Tag: tag, MinLevel: sink.MinLevel})
		if err := syslogconfig.Validate(destination); err != nil {
			c.reject(r, "sinks", "invalid_value", "invalid deletion-time UDP syslog target")
			continue
		}
		policy.Syslog = append(policy.Syslog, destination)
	}
	c.retention = &policy
	c.target(r, "retention")
	c.note(r, "Operational events retain the configured age and daily sweep. Pending consumers/work and current state are protected. Deleted-history deduplication ends with that history. Before-delete UDP archive is local submission, not a receiver acknowledgement. Diagnostic NDJSON logs retain their separate finite rotation.")
}
