package telemetry

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

const ConfigurationApplyMessage = "configuration apply completed"

// ConfigurationApply measures the configuration operation, not subsequent
// protocol acquisition or the completion of a confirmed commit.
type ConfigurationApply struct {
	Revision              uint64
	Result                string
	StartedAt, FinishedAt time.Time
}

func (a ConfigurationApply) Record(ctx context.Context) {
	slog.LogAttrs(ctx, slog.LevelInfo, ConfigurationApplyMessage,
		slog.String("revision", strconv.FormatUint(a.Revision, 10)), slog.String("result", a.Result),
		slog.Time("apply_started_at", a.StartedAt), slog.Time("apply_finished_at", a.FinishedAt),
		slog.Int64("apply_duration_ns", a.FinishedAt.Sub(a.StartedAt).Nanoseconds()))
}
