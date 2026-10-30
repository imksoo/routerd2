package legacyconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestRetentionImportPreservesEventPolicyAndRejectsMissingDatasets(t *testing.T) {
	source := string(fixture(t, "syslog")) + `
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: default}
      spec:
        retention: 7d
        schedule: daily
        vacuum: true
        signals: [events, accessLogs]
        sinks: [LogSink/receiver]
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	p := result.Document.Retention
	if p == nil || p.MaxAgeSeconds != 604800 || p.IntervalSeconds != 86400 || !p.Vacuum || len(p.Syslog) != 1 || p.Syslog[0].Address != "127.0.0.1:5514" {
		t.Fatal(p)
	}
	accessOnly, err := Compile([]byte(strings.Replace(source, "signals: [events, accessLogs]", "signals: [accessLogs]", 1)))
	if err != nil || accessOnly.Document.Retention != nil {
		t.Fatal("inactive access history must not create event retention", accessOnly, err)
	}
	var note string
	for _, mapping := range accessOnly.Mappings {
		if mapping.Resource == "LogRetention/default" {
			note = strings.Join(mapping.Notes, " ")
		}
	}
	if !strings.Contains(note, "access_logs had no producer") {
		t.Fatal("missing explanation for the inactive access history", note)
	}
	for _, signal := range []string{"pluginRuns", "trafficFlows", "firewallEvents", "dhcpFingerprints"} {
		result, err = Compile([]byte(strings.Replace(source, "signals: [events, accessLogs]", "signals: [events, "+signal+"]", 1)))
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || len(result.Document.Resources) != 0 || result.Document.Retention != nil || !strings.Contains(err.Error(), signal) {
			t.Fatal("partial migration reported success", signal, result, err)
		}
	}
}

func TestDNSQueryRetentionImportsIntoTheResolverOwner(t *testing.T) {
	source := string(fixture(t, "dns")) + `
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: queries}
      spec: {retention: 7d, schedule: daily, vacuum: true, signals: [dnsQueries]}
`
	source = strings.Replace(source, "queryLog: {enabled: true}", "queryLog: {enabled: true, path: /legacy/dns-queries.db}", 1)
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.Retention != nil {
		t.Fatal("DNS policy must not prune event history")
	}
	found := false
	for _, r := range result.Document.Resources {
		if r.Service == nil || r.Service.DNS == nil {
			continue
		}
		found = true
		dns := r.Service.DNS
		if dns.QueryRetention == nil || dns.QueryRetention.MaxAgeSeconds != 604800 || dns.QueryRetention.IntervalSeconds != 86400 || !dns.QueryRetention.Vacuum || !dns.Spec.QueryLog.Enabled || dns.Spec.QueryLog.Path != "" {
			t.Fatal(dns)
		}
	}
	if !found {
		t.Fatal("missing resolver")
	}
}

func TestFirewallHistoryRetentionImportsIntoCollector(t *testing.T) {
	source := string(fixture(t, "firewall")) + `
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallEventLog
      metadata: {name: packets}
      spec: {enabled: true, path: /legacy/firewall.db, nflogGroup: 1}
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: packets}
      spec: {retention: 7d, schedule: daily, vacuum: true, signals: [firewallEvents]}
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range result.Document.Resources {
		if resource.Service == nil || resource.Service.FirewallLog == nil {
			continue
		}
		cfg := resource.Service.FirewallLog
		found = true
		if cfg.HistoryPath != "" || cfg.Retention == nil || cfg.Retention.MaxAgeSeconds != 604800 || cfg.Retention.IntervalSeconds != 86400 || !cfg.Retention.Vacuum {
			t.Fatal(cfg)
		}
	}
	if !found || result.Document.Retention != nil {
		t.Fatal("firewall retention was not scoped to its collector")
	}
}

func TestDHCPFingerprintRetentionImportsIntoTheLeaseOwner(t *testing.T) {
	source := string(fixture(t, "dhcp-server")) + `
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: client-identities}
      spec: {retention: 7d, schedule: daily, vacuum: true, signals: [dhcpFingerprints]}
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range result.Document.Resources {
		if resource.Service == nil || resource.Service.DHCPServer == nil {
			continue
		}
		found = true
		policy := resource.Service.DHCPServer.FingerprintRetention
		if policy == nil || policy.MaxAgeSeconds != 604800 || policy.IntervalSeconds != 86400 || !policy.Vacuum {
			t.Fatal(policy)
		}
	}
	if !found || result.Document.Retention != nil {
		t.Fatal("DHCP fingerprint retention was not scoped to the lease owner")
	}
}
