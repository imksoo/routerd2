// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestTelemetryImportPreservesDestinationAndSignalSelection(t *testing.T) {
	result := compiled(t, "otlp")
	got := service(t, result.Document, "otlp_logs").OTLPLogs
	if len(result.Document.Resources) != 1 {
		t.Fatalf("legacy OTLP LogSink created an additional sender: %+v", result.Document.Resources)
	}
	if got.Endpoint != "http://127.0.0.1:4317" || !got.Insecure || got.ServiceNamespace != "edge" || got.Attributes["site"] != "laboratory" {
		t.Fatalf("collector intent changed: %+v", got)
	}
	for _, signals := range []string{"", "        signals: [logs, metrics, traces]"} {
		input := strings.Replace(string(fixture(t, "otlp")), "        signals: [logs]", signals, 1)
		result, err := Compile([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		got := service(t, result.Document, "otlp_logs").OTLPLogs
		if !slices.Equal(got.Signals, []string{"logs", "metrics", "traces"}) {
			t.Fatalf("signals changed: %v", got.Signals)
		}

	}
}
