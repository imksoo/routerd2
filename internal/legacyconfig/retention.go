// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
	"github.com/imksoo/routerd2/internal/retention"
)

func (c *compiler) logRetention(r resource) {
	c.allow(r, "retention", "signals", "sinks", "vacuum", "schedule")
	spec := r.Spec.(api.LogRetentionSpec)
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
	events, queries, firewall, access := false, false, false, false
	for _, signal := range signals {
		if signal == "events" {
			events = true
			continue
		}
		switch signal {
		case "dnsQueries":
			queries = true
		case "firewallEvents":
			firewall = true
		case "accessLogs":
			access = true
			c.note(r, "Legacy access_logs had no producer; retention of that empty dataset creates no native history owner. Existing database contents are not imported.")
		case "pluginRuns", "trafficFlows", "dhcpFingerprints":
			c.reject(r, "signals", "unsupported_semantics", "operational history dataset "+signal+" has no native retention owner")
		default:
			c.reject(r, "signals", "unsupported_semantics", "unknown operational history dataset")
			return
		}
	}
	if firewall {
		enabled := false
		for _, logger := range c.resourcesOfKind("FirewallEventLog") {
			enabled = enabled || logger.Spec.(api.FirewallEventLogSpec).Enabled
		}
		if !enabled {
			c.reject(r, "signals", "unsupported_semantics", "firewallEvents requires an enabled FirewallEventLog history owner")
			return
		}
	}
	if !events && !queries && !firewall && !access {
		c.reject(r, "signals", "unsupported_semantics", "requested datasets have no native history owner; event retention is not substituted")
		return
	}
	if events && c.retention != nil || queries && c.queryRetention != nil || firewall && c.firewallRetention != nil {
		c.reject(r, "", "unsupported_semantics", "each history dataset has one retention policy; combine policies explicitly")
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
		if sink.Type == "otlp" {
			c.note(r, "The legacy OTLP LogSink performed no send; it is not counted as an archive destination. Telemetry remains the independent SDK sender.")
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
	if access && !events && !queries && !firewall {
		c.target(r, "inactive")
	}
	if firewall {
		c.firewallRetention = &policy
		c.target(r, "services/*/firewall_log/retention")
		c.note(r, "Firewall packet history retains the configured age and daily sweep in the existing NFLOG service. Before-delete UDP archive failure preserves expired rows; UDP submission is not receiver acknowledgement. Other requested datasets are not substituted.")
	}
	if events {
		c.retention = &policy
		c.target(r, "retention")
	}
	if queries {
		c.queryRetention = &dnsconfig.QueryRetention{MaxAgeSeconds: policy.MaxAgeSeconds, IntervalSeconds: policy.IntervalSeconds, Vacuum: policy.Vacuum, Syslog: policy.Syslog}
		c.target(r, "services/*/dns/query_log_retention")
		c.note(r, "Each enabled resolver retains its own query database using the configured age, daily sweep, before-delete UDP archive and vacuum. Archive failure leaves expired rows for a later sweep and does not stop DNS replies. UDP submission is not receiver acknowledgement.")
	}
	if events {
		c.note(r, "Operational events retain the configured age and daily sweep. Pending consumers/work and current state are protected. Deleted-history deduplication ends with that history. Before-delete UDP archive is local submission, not a receiver acknowledgement. Diagnostic NDJSON logs retain their separate finite rotation.")
	}
}
