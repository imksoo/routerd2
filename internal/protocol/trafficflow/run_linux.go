// SPDX-License-Identifier: BSD-3-Clause

// Package trafficflow samples connection counters without owning forwarding.
package trafficflow

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/florianl/go-conntrack"
	"github.com/florianl/go-nflog/v2"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
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
	reader, err := conntrack.Open(&conntrack.Config{WriteTimeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer reader.Close()
	var db *sql.DB
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	var packets <-chan capturedPacket
	var packetErrors chan error
	var dropped atomic.Uint64
	pending := make(map[string]classification)
	var native *nativeClassifier
	if cfg.ApplicationLayer {
		native, err = newNativeClassifier(&dropped)
		if err != nil {
			return err
		}
		defer native.close()
	}
	if cfg.PacketGroup != 0 {
		packetReader, err := nflog.Open(&nflog.Config{Group: cfg.PacketGroup, Copymode: nflog.CopyPacket, Bufsize: 2048, QThresh: 1, Flags: nflog.FlagConntrack})
		if err != nil {
			return err
		}
		packetCtx, stopPackets := context.WithCancel(ctx)
		defer func() { stopPackets(); packetReader.Close() }()
		observations := make(chan capturedPacket, 512)
		packetErrors = make(chan error, 1)
		if err := capturePackets(packetCtx, packetReader, observations, packetErrors, &dropped); err != nil {
			return err
		}
		packets = observations
	}

	var nextExpiry time.Time
	ready()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-packetErrors:
			return err
		case packet := <-packets:
			if packet.Unconfirmed {
				delete(pending, packet.Key)
			}
			result, err := classifyPacket(packet, native)
			if err != nil {
				return err
			}
			// A port-based guess is not application evidence from copied packets.
			if (result.Source != "builtin-payload" && result.Source != "libndpi") || result.AppName == "unknown" {
				continue
			}
			if _, exists := pending[packet.Key]; exists || len(pending) < 10000 {
				pending[packet.Key] = classification{Result: result, StartedAt: packet.StartedAt}
			} else {
				dropped.Add(1)
			}
			continue
		case <-timer.C:
		}
		if count := dropped.Swap(0); count != 0 {
			slog.Warn("traffic packet classifications dropped", "count", count)
		}
		// Each sweep is independent. A partial/failed kernel dump must never mark
		// unobserved connections as ended. Disk failure cannot stop forwarding.
		if err := reader.Con.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		entries, err := reader.Dump(conntrack.Conntrack, conntrack.Family(unix.AF_UNSPEC))
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
		observed := make(map[string]struct{}, len(pending))
		for _, entry := range entries {
			flow := trafficFlow(entry, now)
			if flow.FlowKey == "" {
				continue
			}
			key := connectionKey(entry)
			if sample, ok := pending[key]; ok {
				observed[key] = struct{}{}
				if entry.Timestamp != nil && entry.Timestamp.Start != nil && entry.Timestamp.Start.UnixNano() != 0 && sample.StartedAt.IsZero() {
					sample.StartedAt = *entry.Timestamp.Start
					pending[key] = sample
				}
				if sample.StartedAt.IsZero() || sample.StartedAt.Equal(flow.StartedAt) {
					applyClassification(&flow, sample.Result)
				}
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
		if err == nil {
			for key, sample := range pending {
				if _, ok := observed[key]; ok {
					delete(pending, key)
				} else if sample.Missed {
					// The first dump can precede confirmation. A second complete sample
					// without that connection cannot persist these transient observations.
					delete(pending, key)
					dropped.Add(1)
				} else {
					sample.Missed = true
					pending[key] = sample
				}
			}
		}
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

// The kernel identity exists before confirmation. The timestamp additionally
// separates persisted rows across restarts; absent timestamps stay unknown.
func connectionKey(entry conntrack.Con) string {
	if entry.ID == nil || entry.Origin == nil || entry.Origin.Src == nil || entry.Origin.Dst == nil || entry.Origin.Proto == nil || entry.Origin.Proto.Number == nil {
		return ""
	}
	original := entry.Origin
	var source, destination uint16
	if original.Proto.SrcPort != nil {
		source = *original.Proto.SrcPort
	}
	if original.Proto.DstPort != nil {
		destination = *original.Proto.DstPort
	}
	var zone uint16
	if entry.Zone != nil {
		zone = *entry.Zone
	} else if original.Zone != nil {
		zone = *original.Zone
	}
	return logstore.FlowKey(strconv.Itoa(int(*original.Proto.Number)), original.Src.String(), int(source), original.Dst.String(), int(destination)) + "|" + strconv.FormatUint(uint64(zone), 10) + "|" + strconv.FormatUint(uint64(*entry.ID), 10)
}

func trafficFlow(entry conntrack.Con, now time.Time) logstore.TrafficFlow {
	key := connectionKey(entry)
	if key == "" {
		return logstore.TrafficFlow{}
	}
	original, reply := entry.Origin, entry.Reply
	protocol := nl.L4ProtoMap[*original.Proto.Number]
	if protocol == "" {
		protocol = strconv.Itoa(int(*original.Proto.Number))
	}
	flow := logstore.TrafficFlow{StartedAt: now, ClientAddress: original.Src.String(), PeerAddress: original.Dst.String(), Protocol: protocol}
	if original.Proto.SrcPort != nil {
		flow.ClientPort = int(*original.Proto.SrcPort)
	}
	if original.Proto.DstPort != nil {
		flow.PeerPort = int(*original.Proto.DstPort)
	}
	if c := entry.CounterOrigin; c != nil {
		if c.Packets != nil {
			flow.PacketsOut = int64(*c.Packets)
		}
		if c.Bytes != nil {
			flow.BytesOut = int64(*c.Bytes)
		}
	}
	if c := entry.CounterReply; c != nil {
		if c.Packets != nil {
			flow.PacketsIn = int64(*c.Packets)
		}
		if c.Bytes != nil {
			flow.BytesIn = int64(*c.Bytes)
		}
	}
	flow.Accounting = flow.PacketsOut != 0 || flow.PacketsIn != 0
	if reply != nil && reply.Dst != nil && !reply.Dst.Equal(*original.Src) {
		flow.NATTranslatedAddress = reply.Dst.String()
	}
	var started int64
	if entry.Timestamp != nil && entry.Timestamp.Start != nil && entry.Timestamp.Start.UnixNano() != 0 {
		flow.StartedAt = entry.Timestamp.Start.UTC()
		started = flow.StartedAt.UnixNano()
	}
	flow.FlowKey = key + "|" + strconv.FormatInt(started, 10)
	return flow
}
