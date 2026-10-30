// SPDX-License-Identifier: BSD-3-Clause

// Package trafficflow samples connection counters without owning forwarding.
package trafficflow

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func Run(ctx context.Context, cfg trafficflowconfig.Config, ready func()) error {
	if err := trafficflowconfig.Validate(cfg); err != nil {
		return err
	}
	if cfg.HistoryPath == "" {
		return errors.New("traffic flow history path is required")
	}
	reader, err := netlink.NewHandle(unix.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := reader.SetSocketTimeout(2 * time.Second); err != nil {
		return err
	}
	var db *sql.DB
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	var nextExpiry time.Time
	ready()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		// Each sweep is independent. A partial/failed kernel dump must never mark
		// unobserved connections as ended. Disk failure cannot stop forwarding.
		entries, err := reader.ConntrackTableList(netlink.ConntrackTable, unix.AF_UNSPEC)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("traffic flow observation failed", "error", err)
			timer.Reset(cfg.Interval())
			continue
		}
		now := time.Now().UTC()
		flows := make([]logstore.TrafficFlow, 0, len(entries))
		active := make([]string, 0, len(entries))
		for _, entry := range entries {
			flow := trafficFlow(entry, now)
			if flow.FlowKey == "" {
				continue
			}
			flows = append(flows, flow)
			active = append(active, flow.FlowKey)
		}
		call, cancel := context.WithTimeout(ctx, cfg.Interval())
		if db == nil {
			db, err = openHistory(call, cfg.HistoryPath)
		}
		if err == nil {
			err = syncActive(call, db, flows, active, now)
		}
		cancel()
		if err != nil {
			slog.Warn("traffic flow history failed", "error", err)
		} else if cfg.Retention != nil && !now.Before(nextExpiry) {
			call, cancel := context.WithTimeout(ctx, cfg.Interval())
			result, err := expireHistory(call, cfg)
			cancel()
			if err != nil {
				slog.Warn("traffic flow retention failed", "error", err)
			} else if result.Deleted > 0 {
				slog.Info("traffic flow history expired", "deleted", result.Deleted)
			}
			nextExpiry = now.Add(cfg.Retention.Interval())
		}
		timer.Reset(cfg.Interval())
	}
}

// Retain routerd's original/reply direction and stable tuple identity. The
// existing logstore preserves first observation time and cumulative counters.
func trafficFlow(entry *netlink.ConntrackFlow, now time.Time) logstore.TrafficFlow {
	original, reply := entry.Forward, entry.Reverse
	protocol := nl.L4ProtoMap[original.Protocol]
	if protocol == "" {
		protocol = strconv.Itoa(int(original.Protocol))
	}
	flow := logstore.TrafficFlow{
		StartedAt:     now,
		ClientAddress: original.SrcIP.String(), ClientPort: int(original.SrcPort),
		PeerAddress: original.DstIP.String(), PeerPort: int(original.DstPort), Protocol: protocol,
		Accounting: original.Packets != 0 || reply.Packets != 0,
		BytesOut:   int64(original.Bytes), BytesIn: int64(reply.Bytes),
		PacketsOut: int64(original.Packets), PacketsIn: int64(reply.Packets),
	}
	if len(reply.DstIP) != 0 && !reply.DstIP.Equal(original.SrcIP) {
		flow.NATTranslatedAddress = reply.DstIP.String()
	}
	flow.FlowKey = logstore.FlowKey(flow.Protocol, flow.ClientAddress, flow.ClientPort, flow.PeerAddress, flow.PeerPort)
	return flow
}
