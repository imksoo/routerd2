// SPDX-License-Identifier: BSD-3-Clause

package otlplogs

import (
	"context"
	"math"

	"github.com/imksoo/routerd2/internal/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func (p *operationSignals) recordTailscalePeers(ctx context.Context, entry logging.Record) {
	if p.metrics == nil {
		return
	}
	resource, _ := entry.Attributes["resource"].(string)
	count, ok := entry.Attributes["online_peers"].(float64)
	if !ok || resource == "" || count < 0 || count >= 1<<63 || count != math.Trunc(count) {
		return
	}
	p.tailscalePeers.Record(ctx, int64(count), metric.WithAttributes(attribute.String("routerd.tailscale.node", resource)))
}
