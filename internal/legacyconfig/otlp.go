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
	signals := make([]string, 0, len(s.Signals))
	for _, signal := range s.Signals {
		signals = append(signals, strings.TrimSpace(signal))
	}
	if len(signals) == 0 {
		signals = []string{"logs", "metrics", "traces"}
	}
	namespace := s.ServiceNamespace
	if namespace == "" {
		namespace = "routerd"
	}
	cfg := otlplogconfig.Normalize(otlplogconfig.Config{
		Endpoint: s.OTLP.Endpoint, Insecure: s.OTLP.Insecure,
		ServiceName: "routerd2", ServiceNamespace: namespace, Attributes: s.Attributes, Signals: signals,
	})
	if err := otlplogconfig.Validate(cfg); err != nil {
		c.reject(r, "otlp", "invalid_value", "invalid OTLP/gRPC log collector configuration")
		return
	}
	c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "otlp"), OTLPLogs: &cfg})
	c.note(r, "The former active OTLP SDK path exports native local logs through an independent gRPC worker. Endpoint, transport security, namespace and resource attributes are retained. Selected metrics/traces export actual Health probe measurements only; controller, DHCPv6, PPPoE, firewall and other former instrument sources are not yet migrated. Archive-on-delete OTLP LogSink behavior is not supplied.")
}
