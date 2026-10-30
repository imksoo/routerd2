// SPDX-License-Identifier: BSD-3-Clause

package trafficflow

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/florianl/go-conntrack"
	"github.com/florianl/go-nflog/v2"
	"github.com/imksoo/routerd/pkg/dpi"
	"github.com/imksoo/routerd/pkg/logstore"
)

const connectionConfirmed = 1 << 3 // IPS_CONFIRMED, Linux nf_conntrack_status.

type classification struct {
	Result    dpi.ClassifyResult
	StartedAt time.Time
	Missed    bool
}

type capturedPacket struct {
	Key         string
	Payload     []byte
	StartedAt   time.Time
	Unconfirmed bool
}

// NFLOG supplies the same kernel connection identity as the counter dump, even
// before the first packet confirms the connection and its timestamp is set.
// The callback only transfers bounded observations. The worker owns nDPI and
// history; closing this socket never races C state or blocks packet forwarding.
func capturePackets(ctx context.Context, reader *nflog.Nflog, out chan<- capturedPacket, failed chan<- error, dropped *atomic.Uint64) error {
	logger := log.New(io.Discard, "", 0)
	fail := func(err error) int {
		select {
		case failed <- err:
		default:
		}
		return 1
	}
	return reader.RegisterWithErrorFunc(ctx, func(packet nflog.Attribute) int {
		if packet.Ct == nil || packet.Payload == nil {
			return 0
		}
		entry, err := conntrack.ParseAttributes(logger, *packet.Ct)
		if err != nil {
			return fail(err)
		}
		key := connectionKey(entry)
		if key == "" {
			return 0
		}
		sample := capturedPacket{Key: key, Payload: *packet.Payload, Unconfirmed: entry.Status != nil && *entry.Status&connectionConfirmed == 0}
		if entry.Timestamp != nil && entry.Timestamp.Start != nil && entry.Timestamp.Start.UnixNano() != 0 {
			sample.StartedAt = *entry.Timestamp.Start
		}
		select {
		case out <- sample:
		default:
			dropped.Add(1)
		}
		return 0
	}, fail)
}

func classifyPacket(packet capturedPacket, native *nativeClassifier) (dpi.ClassifyResult, error) {
	result := dpi.Classify(dpi.ClassifyRequest{Packet: packet.Payload})
	if native != nil {
		return native.classify(packet, result, time.Now())
	}
	return result, nil
}

func applyClassification(flow *logstore.TrafficFlow, result dpi.ClassifyResult) {
	flow.AppName, flow.AppCategory, flow.AppConfidence = result.AppName, result.AppCategory, result.AppConfidence
	flow.DetectedProtocol, flow.ApplicationProtocol = result.DetectedProtocol, result.ApplicationProtocol
	flow.MasterProtocol, flow.Category, flow.Confidence = result.MasterProtocol, result.Category, result.Confidence
	flow.Engine, flow.Source = result.Engine, result.Source
	flow.Metadata = result.Metadata
	if result.TLSSNI != "" {
		flow.TLSSNI, flow.ResolvedHostname = result.TLSSNI, result.TLSSNI
	}
	if result.HTTPHost != "" {
		flow.HTTPHost, flow.ResolvedHostname = result.HTTPHost, result.HTTPHost
	}
	if result.DNSQuery != "" {
		flow.DNSQuery, flow.ResolvedHostname = result.DNSQuery, result.DNSQuery
	}
}
