// SPDX-License-Identifier: BSD-3-Clause

package otlplogs

import (
	"context"
	"testing"

	"github.com/imksoo/routerd2/internal/logging"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricCollectionDropsUnrefreshedGauge(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(metricTemporality))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(ctx)
	meter := provider.Meter("test")
	gauge, err := meter.Int64Gauge("routerd.tailscale.peer.count")
	if err != nil {
		t.Fatal(err)
	}
	counter, err := meter.Int64Counter("routerd.healthcheck.probes")
	if err != nil {
		t.Fatal(err)
	}
	signals := operationSignals{metrics: provider, tailscalePeers: gauge}
	record := func(count float64) {
		signals.recordTailscalePeers(ctx, logging.Record{Attributes: map[string]any{"resource": "tailnet", "online_peers": count}})
	}
	collect := func(want []int64) {
		t.Helper()
		var result metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &result); err != nil {
			t.Fatal(err)
		}
		var peers []int64
		var probes int64
		for _, scope := range result.ScopeMetrics {
			for _, m := range scope.Metrics {
				switch data := m.Data.(type) {
				case metricdata.Gauge[int64]:
					for _, point := range data.DataPoints {
						peers = append(peers, point.Value)
					}
				case metricdata.Sum[int64]:
					if data.Temporality != metricdata.CumulativeTemporality {
						t.Fatal("counter lost cumulative temporality")
					}
					for _, point := range data.DataPoints {
						probes += point.Value
					}
				}
			}
		}
		if len(peers) != len(want) {
			t.Fatalf("peer measurements = %v, want %v", peers, want)
		}
		for i := range want {
			if peers[i] != want[i] {
				t.Fatalf("peer measurements = %v, want %v", peers, want)
			}
		}
		if probes != 2 {
			t.Fatalf("probe total = %d, want 2", probes)
		}
	}
	counter.Add(ctx, 2)
	record(1)
	collect([]int64{1})
	collect(nil) // Removing or stopping the producer must not refresh its last value.
	record(0)
	collect([]int64{0})
	collect(nil)
	record(1)
	collect([]int64{1})
}
