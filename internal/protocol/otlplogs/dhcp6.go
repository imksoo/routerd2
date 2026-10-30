// SPDX-License-Identifier: BSD-3-Clause

package otlplogs

import (
	"context"
	"time"

	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func (p *operationSignals) recordDHCPv6(ctx context.Context, entry logging.Record) {
	text := func(key string) string { value, _ := entry.Attributes[key].(string); return value }
	exchange := telemetry.DHCPv6Exchange{Resource: text("resource"), Interface: text("interface"), Operation: text("operation"), Result: text("result")}
	var err error
	exchange.StartedAt, err = time.Parse(time.RFC3339Nano, text("exchange_started_at"))
	if err != nil {
		return
	}
	exchange.FinishedAt, err = time.Parse(time.RFC3339Nano, text("exchange_finished_at"))
	if err != nil || exchange.Resource == "" || exchange.Operation == "" || exchange.Result == "" || exchange.FinishedAt.Before(exchange.StartedAt) {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("routerd.resource.name", exchange.Resource), attribute.String("network.interface.name", exchange.Interface), attribute.String("routerd.dhcpv6.operation", exchange.Operation), attribute.String("routerd.dhcpv6.result", exchange.Result)}
	if p.metrics != nil {
		p.exchanges.Add(ctx, 1, metric.WithAttributes(attrs...))
		p.exchangeDuration.Record(ctx, exchange.FinishedAt.Sub(exchange.StartedAt).Seconds(), metric.WithAttributes(attrs...))
	}
	if p.traces != nil {
		transmissions, _ := entry.Attributes["transmissions"].(float64)
		attrs = append(attrs, attribute.String("network.protocol.name", "dhcpv6"), attribute.Int64("routerd.dhcpv6.transmissions", int64(transmissions)), attribute.String("routerd.log.epoch", entry.Epoch), attribute.Int64("routerd.log.sequence", int64(entry.Sequence)))
		_, span := p.dhcp6Tracer.Start(ctx, "dhcpv6.exchange", trace.WithTimestamp(exchange.StartedAt), trace.WithAttributes(attrs...))
		switch exchange.Result {
		case "timeout", "expired", "no_binding", "error":
			span.SetStatus(codes.Error, exchange.Result)
		}
		span.End(trace.WithTimestamp(exchange.FinishedAt))
	}
}
