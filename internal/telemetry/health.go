// SPDX-License-Identifier: BSD-3-Clause

// Package telemetry contains actual operation measurements sent through the
// existing bounded local log transport. It does not own protocol state.
package telemetry

import (
	"context"
	"log/slog"
	"time"
)

const HealthProbeMessage = "health probe completed"

type HealthProbe struct {
	Resource, Interface, Target, Protocol, Result, Phase string
	StartedAt, FinishedAt                                time.Time
}

func (p HealthProbe) Record(ctx context.Context) {
	slog.LogAttrs(ctx, slog.LevelInfo, HealthProbeMessage,
		slog.String("resource", p.Resource), slog.String("interface", p.Interface),
		slog.String("target", p.Target), slog.String("protocol", p.Protocol),
		slog.String("result", p.Result), slog.String("phase", p.Phase),
		slog.Time("probe_started_at", p.StartedAt), slog.Time("probe_finished_at", p.FinishedAt))
}
