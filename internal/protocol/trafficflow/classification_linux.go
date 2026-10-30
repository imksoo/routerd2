// SPDX-License-Identifier: BSD-3-Clause

package trafficflow

import (
	"context"
	"sync/atomic"

	"github.com/imksoo/routerd/pkg/dpi"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd/pkg/nflog"
	"github.com/vishvananda/netlink"
)

// Packet decoding/classification remains routerd's implementation. The worker
// owns this reader; its bounded queue never holds the forwarding path or disk.
func collectClassifications(ctx context.Context, reader *nflog.Reader, out chan<- dpi.ClassifyResult, dropped *atomic.Uint64) error {
	defer close(out)
	for ctx.Err() == nil {
		packet, err := reader.Read(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		result := dpi.Classify(dpi.ClassifyRequest{Packet: packet.Payload})
		// A transport port alone is not packet-derived application evidence.
		if result.Source != "builtin-payload" || result.AppName == "unknown" {
			continue
		}
		select {
		case out <- result:
		default:
			dropped.Add(1)
		}
	}
	return nil
}

func applyClassification(flow *logstore.TrafficFlow, entry *netlink.ConntrackFlow, pending map[string]dpi.ClassifyResult) {
	// At FORWARD, destination NAT has happened but source NAT has not. Reading
	// original source and reply source also handles translated destination ports.
	a, b := entry.Forward, entry.Reverse
	for _, key := range []string{
		logstore.FlowKey(flow.Protocol, a.SrcIP.String(), int(a.SrcPort), b.SrcIP.String(), int(b.SrcPort)),
		logstore.FlowKey(flow.Protocol, b.SrcIP.String(), int(b.SrcPort), a.SrcIP.String(), int(a.SrcPort)),
	} {
		if result, ok := pending[key]; ok {
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
	}
}
