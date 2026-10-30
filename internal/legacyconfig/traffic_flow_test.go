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
	for _, flag := range []string{"includeApplicationLayer"} {
		result, err := Compile([]byte(strings.Replace(source, "source: conntrack", "source: conntrack, "+flag+": true", 1)))
		if err == nil || len(result.Document.Resources) != 0 || !strings.Contains(err.Error(), "nDPI") {
			t.Fatal("classification request was discarded", flag, err)
		}
	}
}

func TestTrafficFlowSNIImportsOnePacketOwner(t *testing.T) {
	source := `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: flows}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: TrafficFlowLog
      metadata: {name: connections}
      spec: {enabled: true, source: conntrack, includeTLSSNI: true}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallEventLog
      metadata: {name: firewall}
      spec: {enabled: true, nflogGroup: 2}
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	var group uint16
	for _, r := range result.Document.Resources {
		if r.Service != nil && r.Service.TrafficFlow != nil {
			group = r.Service.TrafficFlow.PacketGroup
		}
	}
	if group != 3 {
		t.Fatal("collector group collides with firewall history", group)
	}
	for _, r := range result.Document.Resources {
		if r.Firewall != nil && r.Firewall.FlowObservationGroup == group {
			return
		}
	}
	t.Fatal("packet observation was not connected to the native firewall")
}
