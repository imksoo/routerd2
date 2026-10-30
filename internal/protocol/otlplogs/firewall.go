// SPDX-License-Identifier: BSD-3-Clause

package otlplogs

import (
	"context"

	"github.com/imksoo/routerd2/internal/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Count actual NFLOG observations from the existing packet log owner. Accepted
// packets and unknown verdicts do not become denies. Packet sampling and local
// log loss are not reconstructed as unobserved traffic.
func (p *operationSignals) recordFirewall(ctx context.Context, entry logging.Record) {
	if p.metrics == nil {
		return
	}
	text := func(key string) string { value, _ := entry.Attributes[key].(string); return value }
	action := text("action")
	if action != "drop" && action != "reject" {
		return
	}
	p.deniedPackets.Add(ctx, 1, metric.WithAttributes(
		attribute.String("routerd.firewall.rule", text("rule")),
		attribute.String("routerd.firewall.action", action),
		attribute.String("network.transport", text("protocol")),
		attribute.String("network.type", text("family")),
	))
}
