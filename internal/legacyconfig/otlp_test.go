// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestOTLPLogsImportPreservesDestinationAndRejectsMissingSignals(t *testing.T) {
	result := compiled(t, "otlp")
	got := service(t, result.Document, "otlp_logs").OTLPLogs
	if got.Endpoint != "http://127.0.0.1:4317" || !got.Insecure || got.ServiceNamespace != "edge" || got.Attributes["site"] != "laboratory" {
		t.Fatalf("collector intent changed: %+v", got)
	}
	for _, signals := range []string{"", "        signals: [logs, metrics, traces]"} {
		input := strings.Replace(string(fixture(t, "otlp")), "        signals: [logs]", signals, 1)
		result, err := Compile([]byte(input))
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || len(result.Document.Resources) != 0 {
			t.Fatalf("unimplemented signals were discarded: %v", err)
		}
	}
}
