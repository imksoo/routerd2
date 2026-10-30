// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"strings"
	"testing"
)

func TestTrafficFlowImportRetainsHistoryAndRejectsMissingClassification(t *testing.T) {
	source := `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: flows}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: TrafficFlowLog
      metadata: {name: connections}
      spec: {enabled: true, source: conntrack, path: /legacy/flows.db}
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: flows}
      spec: {retention: 7d, schedule: daily, vacuum: true, signals: [trafficFlows]}
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	cfg := result.Document.Resources[0].Service.TrafficFlow
	if cfg == nil || cfg.HistoryPath != "" || cfg.Retention == nil || cfg.Retention.MaxAgeSeconds != 604800 || cfg.Retention.IntervalSeconds != 86400 || !cfg.Retention.Vacuum || result.Document.Retention != nil {
		t.Fatal("flow retention was not scoped to its collector", result)
	}
	for _, flag := range []string{"includeApplicationLayer", "includeTLSSNI"} {
		result, err := Compile([]byte(strings.Replace(source, "source: conntrack", "source: conntrack, "+flag+": true", 1)))
		if err == nil || len(result.Document.Resources) != 0 || !strings.Contains(err.Error(), "DPI/TLS") {
			t.Fatal("classification request was discarded", flag, err)
		}
	}
}
