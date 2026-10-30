// SPDX-License-Identifier: BSD-3-Clause

//go:build linux && cgo && libndpi

package trafficflow

/*
#cgo pkg-config: libndpi
#include <stdlib.h>
#include <ndpi_api.h>

static struct ndpi_detection_module_struct* routerd_ndpi_init(void) {
#if defined(NDPI_MAJOR) && NDPI_MAJOR >= 5
	struct ndpi_detection_module_struct *mod = ndpi_init_detection_module(NULL);
#else
	NDPI_PROTOCOL_BITMASK all;
	struct ndpi_detection_module_struct *mod = ndpi_init_detection_module(ndpi_no_prefs);
#endif
	if(mod == NULL) return NULL;
#if !defined(NDPI_MAJOR) || NDPI_MAJOR < 5
	NDPI_BITMASK_SET_ALL(all);
	ndpi_set_protocol_detection_bitmask2(mod, &all);
#endif
	ndpi_finalize_initialization(mod);
	return mod;
}

static struct ndpi_flow_struct* routerd_ndpi_new_flow(void) {
	return (struct ndpi_flow_struct*)calloc(1, sizeof(struct ndpi_flow_struct));
}

static ndpi_protocol routerd_ndpi_process(struct ndpi_detection_module_struct *mod,
					  struct ndpi_flow_struct *flow,
					  const unsigned char *packet,
					  unsigned short packetlen,
					  unsigned long long now_ms) {
#if defined(NDPI_MAJOR) && NDPI_MAJOR >= 5
	return ndpi_detection_process_packet(mod, flow, packet, packetlen, now_ms, NULL);
#else
	return ndpi_detection_process_packet(mod, flow, packet, packetlen, now_ms);
#endif
}

static ndpi_protocol routerd_ndpi_giveup(struct ndpi_detection_module_struct *mod,
					 struct ndpi_flow_struct *flow) {
#if defined(NDPI_MAJOR) && NDPI_MAJOR >= 5
	return ndpi_detection_giveup(mod, flow);
#else
	u_int8_t guessed = 0;
	return ndpi_detection_giveup(mod, flow, 1, &guessed);
#endif
}

static u_int16_t routerd_ndpi_master_protocol(ndpi_protocol proto) {
#if defined(NDPI_MAJOR) && NDPI_MAJOR >= 5
	return proto.proto.master_protocol;
#else
	return proto.master_protocol;
#endif
}

static u_int16_t routerd_ndpi_app_protocol(ndpi_protocol proto) {
#if defined(NDPI_MAJOR) && NDPI_MAJOR >= 5
	return proto.proto.app_protocol;
#else
	return proto.app_protocol;
#endif
}

static int routerd_ndpi_confidence(struct ndpi_flow_struct *flow) {
	return (int)flow->confidence;
}

static const char* routerd_ndpi_hostname(struct ndpi_flow_struct *flow) {
	return flow->host_server_name;
}
*/
import "C"

import (
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd/pkg/dpi"
	"github.com/imksoo/routerd/pkg/logstore"
)

// The packet reader alone owns the module and its C flows. These are routerd's
// libndpi API calls and result conversion, without its agent process/HTTP layer.
type nativeClassifier struct {
	mod        *C.struct_ndpi_detection_module_struct
	flows      map[string]*nativeFlow
	version    string
	nextExpiry time.Time
	dropped    *atomic.Uint64
}

type nativeFlow struct {
	value    *C.struct_ndpi_flow_struct
	packets  int
	lastSeen time.Time
	result   dpi.ClassifyResult
}

func newNativeClassifier(dropped *atomic.Uint64) (*nativeClassifier, error) {
	mod := C.routerd_ndpi_init()
	if mod == nil {
		return nil, errors.New("libndpi initialization failed")
	}
	c := &nativeClassifier{mod: mod, flows: make(map[string]*nativeFlow), version: C.GoString(C.ndpi_revision()), dropped: dropped}
	slog.Info("traffic application classifier initialized", "engine", "ndpi", "library", "libndpi", "version", c.version)
	return c, nil
}

func (c *nativeClassifier) close() {
	for _, flow := range c.flows {
		if flow.value != nil {
			C.ndpi_free_flow(flow.value)
		}
	}
	C.ndpi_exit_detection_module(c.mod)
}

func (c *nativeClassifier) classify(packet []byte, result dpi.ClassifyResult, now time.Time) (dpi.ClassifyResult, error) {
	if len(packet) == 0 || len(packet) > 0xffff || result.SrcAddress == "" || result.DstAddress == "" {
		return result, nil
	}
	// Both directions of the same copied connection use one nDPI flow state.
	a := logstore.FlowKey(result.TransportProtocol, result.SrcAddress, result.SrcPort, result.DstAddress, result.DstPort)
	b := logstore.FlowKey(result.TransportProtocol, result.DstAddress, result.DstPort, result.SrcAddress, result.SrcPort)
	key := min(a, b)
	if !now.Before(c.nextExpiry) {
		for key, flow := range c.flows {
			if now.Sub(flow.lastSeen) >= time.Hour {
				if flow.value != nil {
					C.ndpi_free_flow(flow.value)
				}
				delete(c.flows, key)
			}
		}
		c.nextExpiry = now.Add(time.Minute)
	}
	flow := c.flows[key]
	if flow == nil {
		// Reuse routerd's oldest-flow eviction at the collector's bounded capacity.
		// Eviction frees unfinished C state; later traffic can still be classified.
		if len(c.flows) >= 10000 {
			var oldestKey string
			var oldest time.Time
			for k, candidate := range c.flows {
				if oldestKey == "" || candidate.lastSeen.Before(oldest) {
					oldestKey, oldest = k, candidate.lastSeen
				}
			}
			victim := c.flows[oldestKey]
			if victim.value != nil {
				C.ndpi_free_flow(victim.value)
			}
			delete(c.flows, oldestKey)
			c.dropped.Add(1)
		}
		flow = &nativeFlow{value: C.routerd_ndpi_new_flow()}
		if flow.value == nil {
			return result, errors.New("libndpi flow allocation failed")
		}
		c.flows[key] = flow
	}
	flow.lastSeen = now
	if flow.value != nil {
		flow.packets++
		raw := C.CBytes(packet)
		proto := C.routerd_ndpi_process(c.mod, flow.value, (*C.uchar)(raw), C.ushort(len(packet)), C.ulonglong(now.UnixMilli()))
		C.free(raw)
		// The existing observation rule copies at most nine packets per connection.
		if flow.packets >= 9 && C.routerd_ndpi_app_protocol(proto) == C.NDPI_PROTOCOL_UNKNOWN && C.routerd_ndpi_master_protocol(proto) == C.NDPI_PROTOCOL_UNKNOWN {
			proto = C.routerd_ndpi_giveup(c.mod, flow.value)
		}
		detected := c.resultFromProtocol(flow.value, proto)
		if detected.AppName != "unknown" {
			flow.result = detected
			C.ndpi_free_flow(flow.value)
			flow.value = nil
		} else if flow.packets >= 9 {
			C.ndpi_free_flow(flow.value)
			delete(c.flows, key)
		}
	}
	if flow.result.Engine == "ndpi" {
		detected := flow.result
		detected.SrcAddress, detected.SrcPort = result.SrcAddress, result.SrcPort
		detected.DstAddress, detected.DstPort = result.DstAddress, result.DstPort
		detected.TransportProtocol = result.TransportProtocol
		detected.Metadata = maps.Clone(detected.Metadata)
		if result.TLSSNI != "" {
			detected.TLSSNI = result.TLSSNI
		}
		if result.HTTPHost != "" {
			detected.HTTPHost = result.HTTPHost
		}
		if result.DNSQuery != "" {
			detected.DNSQuery = result.DNSQuery
		}
		return dpi.FinalizeResult(detected), nil
	}
	return result, nil
}

func (c *nativeClassifier) resultFromProtocol(flow *C.struct_ndpi_flow_struct, proto C.ndpi_protocol) dpi.ClassifyResult {
	result := dpi.ClassifyResult{AppName: "unknown", Engine: "ndpi", Source: "libndpi", Metadata: map[string]string{"libndpi.version": c.version}}
	masterProto := C.routerd_ndpi_master_protocol(proto)
	appProto := C.routerd_ndpi_app_protocol(proto)
	if masterProto != C.NDPI_PROTOCOL_UNKNOWN {
		result.MasterProtocol = strings.ToLower(C.GoString(C.ndpi_get_proto_name(c.mod, C.u_int16_t(masterProto))))
	}
	if appProto != C.NDPI_PROTOCOL_UNKNOWN {
		result.ApplicationProtocol = strings.ToLower(C.GoString(C.ndpi_get_proto_name(c.mod, C.u_int16_t(appProto))))
	}
	detectedProto := appProto
	if detectedProto == C.NDPI_PROTOCOL_UNKNOWN {
		detectedProto = masterProto
	}
	if detectedProto == C.NDPI_PROTOCOL_UNKNOWN {
		result.Reason = "libndpi_unknown"
		return dpi.FinalizeResult(result)
	}
	result.DetectedProtocol = strings.ToLower(C.GoString(C.ndpi_get_proto_name(c.mod, C.u_int16_t(detectedProto))))
	result.AppName = result.DetectedProtocol
	result.AppCategory = strings.ToLower(C.GoString(C.ndpi_category_get_name(c.mod, C.ndpi_get_proto_category(c.mod, proto))))
	switch int(C.routerd_ndpi_confidence(flow)) {
	case int(C.NDPI_CONFIDENCE_DPI):
		result.AppConfidence = 95
	case int(C.NDPI_CONFIDENCE_DPI_CACHE):
		result.AppConfidence = 85
	case int(C.NDPI_CONFIDENCE_MATCH_BY_IP):
		result.AppConfidence = 65
	case int(C.NDPI_CONFIDENCE_MATCH_BY_PORT):
		result.AppConfidence = 45
	}
	host := C.GoString(C.routerd_ndpi_hostname(flow))
	if host != "" {
		switch result.AppName {
		case "http":
			result.HTTPHost = host
		default:
			result.TLSSNI = host
		}
	}
	result.Reason = "libndpi_protocol_detection"
	return dpi.FinalizeResult(result)
}
