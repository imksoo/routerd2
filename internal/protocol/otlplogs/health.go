// SPDX-License-Identifier: BSD-3-Clause

package otlplogs

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/otlplogconfig"
	"github.com/imksoo/routerd2/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

type operationSignals struct {
	metrics          *sdkmetric.MeterProvider
	traces           *sdktrace.TracerProvider
	probes           metric.Int64Counter
	phase            metric.Int64Gauge
	tracer           trace.Tracer
	exchanges        metric.Int64Counter
	exchangeDuration metric.Float64Histogram
	dhcp6Tracer      trace.Tracer
	deniedPackets    metric.Int64Counter
}

func newOperationSignals(ctx context.Context, cfg otlplogconfig.Config, conn *grpc.ClientConn, res *resource.Resource) (p *operationSignals, err error) {
	metrics, traces := slices.Contains(cfg.Signals, "metrics"), slices.Contains(cfg.Signals, "traces")
	if !metrics && !traces {
		return nil, nil
	}
	p = &operationSignals{}
	if metrics {
		exporter, e := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn), otlpmetricgrpc.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector), otlpmetricgrpc.WithEndpoint(conn.Target()), otlpmetricgrpc.WithHeaders(cfg.Headers), otlpmetricgrpc.WithCompressor("none"), otlpmetricgrpc.WithTimeout(operationTimeout), otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}))
		if e != nil {
			return p, e
		}
		p.metrics = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(30*time.Second), sdkmetric.WithTimeout(operationTimeout))))
		meter := p.metrics.Meter("routerd2.healthcheck")
		p.probes, err = meter.Int64Counter("routerd.healthcheck.probes")
		if err != nil {
			return p, err
		}
		p.phase, err = meter.Int64Gauge("routerd.healthcheck.phase")
		if err != nil {
			return p, err
		}
		dhcp6Meter := p.metrics.Meter("routerd2.dhcpv6")
		p.exchanges, err = dhcp6Meter.Int64Counter("routerd.dhcpv6.client.exchanges")
		if err != nil {
			return p, err
		}
		p.exchangeDuration, err = dhcp6Meter.Float64Histogram("routerd.dhcpv6.client.exchange.duration", metric.WithUnit("s"))
		if err != nil {
			return p, err
		}
		p.deniedPackets, err = p.metrics.Meter("routerd2.firewall").Int64Counter("routerd.firewall.deny.total", metric.WithUnit("{packet}"))
		if err != nil {
			return p, err
		}
	}
	if traces {
		exporter, e := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn), otlptracegrpc.WithEndpoint(conn.Target()), otlptracegrpc.WithHeaders(cfg.Headers), otlptracegrpc.WithCompressor("none"), otlptracegrpc.WithTimeout(operationTimeout), otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}))
		if e != nil {
			return p, e
		}
		p.traces = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(1024), sdktrace.WithMaxExportBatchSize(256), sdktrace.WithBatchTimeout(250*time.Millisecond), sdktrace.WithExportTimeout(operationTimeout)))
		p.tracer = p.traces.Tracer("routerd2.healthcheck")
		p.dhcp6Tracer = p.traces.Tracer("routerd2.dhcpv6")
	}
	return p, nil
}

func (p *operationSignals) close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var err error
	if p.traces != nil {
		err = p.traces.Shutdown(ctx)
	}
	if p.metrics != nil {
		err = errors.Join(err, p.metrics.Shutdown(ctx))
	}
	return err
}

func (p *operationSignals) record(ctx context.Context, entry logging.Record) {
	switch entry.Message {
	case telemetry.HealthProbeMessage:
		p.recordHealth(ctx, entry)
	case telemetry.DHCPv6ExchangeMessage:
		p.recordDHCPv6(ctx, entry)
	case "firewall packet":
		p.recordFirewall(ctx, entry)
	}
}

func (p *operationSignals) recordHealth(ctx context.Context, entry logging.Record) {
	// Narrow the existing JSON-scalar logging boundary to the actual measurement.
	text := func(key string) string { value, _ := entry.Attributes[key].(string); return value }
	probe := telemetry.HealthProbe{Resource: text("resource"), Interface: text("interface"), Target: text("target"), Protocol: text("protocol"), Result: text("result"), Phase: text("phase")}
	var err error
	probe.StartedAt, err = time.Parse(time.RFC3339Nano, text("probe_started_at"))
	if err != nil {
		return
	}
	probe.FinishedAt, err = time.Parse(time.RFC3339Nano, text("probe_finished_at"))
	if err != nil || probe.Resource == "" || probe.FinishedAt.Before(probe.StartedAt) {
		return
	}
	if p.metrics != nil {
		p.probes.Add(ctx, 1, metric.WithAttributes(attribute.String("routerd.resource.name", probe.Resource), attribute.String("routerd.healthcheck.result", probe.Result), attribute.String("server.address", probe.Target), attribute.String("network.protocol.name", probe.Protocol)))
		var phase int64
		switch probe.Phase {
		case "Healthy":
			phase = 2
		case "Passing":
			phase = 1
		case "Failing":
			phase = -1
		case "Unhealthy":
			phase = -2
		}
		p.phase.Record(ctx, phase, metric.WithAttributes(attribute.String("routerd.resource.name", probe.Resource), attribute.String("routerd.healthcheck.phase", probe.Phase)))
	}
	if p.traces != nil {
		_, span := p.tracer.Start(ctx, "healthcheck.probe", trace.WithTimestamp(probe.StartedAt), trace.WithAttributes(
			attribute.String("routerd.resource.kind", "HealthCheck"), attribute.String("routerd.resource.name", probe.Resource), attribute.String("network.protocol.name", probe.Protocol), attribute.String("server.address", probe.Target), attribute.String("network.interface.name", probe.Interface), attribute.String("routerd.healthcheck.result", probe.Result), attribute.String("routerd.log.epoch", entry.Epoch), attribute.Int64("routerd.log.sequence", int64(entry.Sequence))))
		span.End(trace.WithTimestamp(probe.FinishedAt))
	}
}
