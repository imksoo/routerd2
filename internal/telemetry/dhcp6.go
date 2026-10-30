// SPDX-License-Identifier: BSD-3-Clause

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

const DHCPv6ExchangeMessage = "dhcpv6 exchange completed"

type DHCPv6Exchange struct {
	Resource, Interface, Operation, Result string
	StartedAt, FinishedAt                  time.Time
	Transmissions                          int
}

func (e DHCPv6Exchange) Record(ctx context.Context) {
	slog.LogAttrs(ctx, slog.LevelInfo, DHCPv6ExchangeMessage,
		slog.String("resource", e.Resource), slog.String("interface", e.Interface),
		slog.String("operation", e.Operation), slog.String("result", e.Result),
		slog.Time("exchange_started_at", e.StartedAt), slog.Time("exchange_finished_at", e.FinishedAt),
		slog.Int("transmissions", e.Transmissions))
}
