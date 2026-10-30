// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/otlplogconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// Telemetry is the old active SDK path. LogSink's OTLPSink.Emit was a no-op.
func (c *compiler) telemetry(r resource) {
	c.allow(r, "otlp", "serviceNamespace", "attributes", "signals")
	s := r.Spec.(api.TelemetrySpec)
	if len(s.Signals) == 0 {
		c.reject(r, "signals", "unsupported_semantics", "omitted Telemetry signals enable logs, metrics and traces in routerd; only explicit logs-only export is implemented")
		return
	}
	for _, signal := range s.Signals {
		if strings.TrimSpace(signal) != "logs" {
			c.reject(r, "signals", "unsupported_semantics", "OTLP metrics and traces are not implemented; they cannot be discarded during import")
			return
		}
	}
	namespace := s.ServiceNamespace
	if namespace == "" {
		namespace = "routerd"
	}
	cfg := otlplogconfig.Normalize(otlplogconfig.Config{
		Endpoint: s.OTLP.Endpoint, Insecure: s.OTLP.Insecure,
		ServiceName: "routerd2", ServiceNamespace: namespace, Attributes: s.Attributes,
	})
	if err := otlplogconfig.Validate(cfg); err != nil {
		c.reject(r, "otlp", "invalid_value", "invalid OTLP/gRPC log collector configuration")
		return
	}
	c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "otlp"), OTLPLogs: &cfg})
	c.note(r, "The former active OTLP SDK path exports native local logs through an independent gRPC worker. Endpoint, transport security, namespace and resource attributes are retained. Metrics/traces and archive-on-delete LogSink behavior are not supplied.")
}
