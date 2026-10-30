// SPDX-License-Identifier: BSD-3-Clause

// Package firewalllog records kernel NFLOG packets through the normal log owner.
package firewalllog

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd/pkg/nflog"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
)

func Run(ctx context.Context, cfg firewalllogconfig.Config, ready func()) error {
	if err := firewalllogconfig.Validate(cfg); err != nil {
		return err
	}
	if cfg.HistoryPath == "" {
		return errors.New("firewall history path is required")
	}
	db, err := openHistory(cfg.HistoryPath)
	if err != nil {
		return err
	}
	defer db.Close()
	historyCtx, stopHistory := context.WithCancel(context.WithoutCancel(ctx))
	entries := make(chan logstore.FirewallLogEntry, 512)
	var dropped atomic.Uint64
	done := make(chan struct{})
	go func() { defer close(done); retainHistory(historyCtx, cfg, db, entries, &dropped) }()
	defer func() { stopHistory(); <-done }()
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
		entry := logstore.FirewallLogEntry{Timestamp: packet.Timestamp, RuleName: strings.TrimSpace(prefix), Action: action, SrcAddress: packet.SrcAddress, SrcPort: packet.SrcPort, DstAddress: packet.DstAddress, DstPort: packet.DstPort, Protocol: packet.Protocol, TCPFlags: tcpFlagsFromIPPacket(packet.Payload), L3Proto: packet.L3Proto, InIface: packet.InIface, OutIface: packet.OutIface, PacketBytes: packet.PacketBytes, Hint: "nflog-netlink"}
		select {
		case entries <- entry:
		default:
			dropped.Add(1)
		}
		slog.LogAttrs(ctx, slog.LevelInfo, "firewall packet",
			slog.String("action", action), slog.String("rule", strings.TrimSpace(prefix)),
			slog.Time("packet_timestamp", packet.Timestamp), slog.Int("group", int(cfg.Group)),
			slog.String("source_address", packet.SrcAddress), slog.Int("source_port", packet.SrcPort),
			slog.String("destination_address", packet.DstAddress), slog.Int("destination_port", packet.DstPort),
			slog.String("protocol", packet.Protocol), slog.String("tcp_flags", entry.TCPFlags), slog.String("family", packet.L3Proto),
			slog.String("in_interface", packet.InIface), slog.String("out_interface", packet.OutIface),
			slog.Int("packet_bytes", packet.PacketBytes))
	}
	return nil
}
