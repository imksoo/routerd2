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
        signals: [events]
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
	for _, signal := range []string{"accessLogs", "pluginRuns", "dnsQueries", "trafficFlows", "firewallEvents", "dhcpFingerprints"} {
		result, err = Compile([]byte(strings.Replace(source, "signals: [events]", "signals: [events, "+signal+"]", 1)))
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || len(result.Document.Resources) != 0 || result.Document.Retention != nil || !strings.Contains(err.Error(), signal) {
			t.Fatal("partial migration reported success", signal, result, err)
		}
	}
}
