// SPDX-License-Identifier: BSD-3-Clause

package telemetry

import (
	"context"
	"log/slog"
)

const TailscalePeersMessage = "Tailscale peers observed"

// TailscalePeers measures the official daemon's Online peer flags, excluding
// self. It is not a count of current connections or successful handshakes.
type TailscalePeers struct {
	Resource    string
	OnlinePeers int
}

func (p TailscalePeers) Record(ctx context.Context) {
	slog.LogAttrs(ctx, slog.LevelInfo, TailscalePeersMessage,
		slog.String("resource", p.Resource), slog.Int("online_peers", p.OnlinePeers))
}
