package otlplogs

import (
	"context"
	"time"

	"github.com/imksoo/routerd2/internal/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func (p *operationSignals) recordConfigurationApply(ctx context.Context, entry logging.Record) {
	text := func(key string) string { value, _ := entry.Attributes[key].(string); return value }
	started, err := time.Parse(time.RFC3339Nano, text("apply_started_at"))
	if err != nil {
		return
	}
	finished, err := time.Parse(time.RFC3339Nano, text("apply_finished_at"))
	if err != nil {
		return
	}
	duration, ok := entry.Attributes["apply_duration_ns"].(float64)
	result := text("result")
	if !ok || duration < 0 || (result != "success" && result != "error") {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("routerd.config.apply.result", result)}
	if p.metrics != nil {
		p.configApplies.Add(ctx, 1, metric.WithAttributes(attrs...))
		// The source records monotonic elapsed time, independent of clock steps.
		p.configApplyDuration.Record(ctx, duration/float64(time.Second), metric.WithAttributes(attrs...))
	}
	if p.traces != nil {
		attrs = append(attrs, attribute.String("routerd.config.revision", text("revision")),
			attribute.String("routerd.log.epoch", entry.Epoch), attribute.Int64("routerd.log.sequence", int64(entry.Sequence)))
		_, span := p.configTracer.Start(ctx, "config.apply", trace.WithTimestamp(started), trace.WithAttributes(attrs...))
		if result == "error" {
			span.SetStatus(codes.Error, "configuration apply failed")
		}
		span.End(trace.WithTimestamp(finished))
	}
}
