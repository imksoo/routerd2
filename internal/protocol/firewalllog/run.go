// SPDX-License-Identifier: BSD-3-Clause

// Package firewalllog records kernel NFLOG packets through the normal log owner.
package firewalllog

import (
	"context"
	"log/slog"
	"strings"

	"github.com/imksoo/routerd/pkg/nflog"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
)

func Run(ctx context.Context, cfg firewalllogconfig.Config, ready func()) error {
	reader, err := nflog.Open(int(cfg.Group))
	if err != nil {
		return err
	}
	defer reader.Close()
	ready()
	for ctx.Err() == nil {
		packet, err := reader.Read(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		action, prefix, ok := strings.Cut(packet.Prefix, " ")
		if !ok || action != "drop" && action != "reject" && action != "accept" {
			action, prefix = "unknown", packet.Prefix
		}
		slog.LogAttrs(ctx, slog.LevelInfo, "firewall packet",
			slog.String("action", action), slog.String("rule", strings.TrimSpace(prefix)),
			slog.Time("packet_timestamp", packet.Timestamp), slog.Int("group", int(cfg.Group)),
			slog.String("source_address", packet.SrcAddress), slog.Int("source_port", packet.SrcPort),
			slog.String("destination_address", packet.DstAddress), slog.Int("destination_port", packet.DstPort),
			slog.String("protocol", packet.Protocol), slog.String("family", packet.L3Proto),
			slog.String("in_interface", packet.InIface), slog.String("out_interface", packet.OutIface),
			slog.Int("packet_bytes", packet.PacketBytes))
	}
	return nil
}
