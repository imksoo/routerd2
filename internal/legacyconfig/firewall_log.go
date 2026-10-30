// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"math"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// The old renderer routes existing rule/policy log expressions into NFLOG.
// Its logger does not consume FirewallEventLog.sinks or log.reject.
func (c *compiler) firewallLog() network.PacketLog {
	var logging network.PacketLog
	for _, r := range c.resourcesOfKind("FirewallEventLog") {
		c.allow(r, "enabled", "path", "sinks", "nflogGroup", "log")
		s := r.Spec.(api.FirewallEventLogSpec)
		if !s.Enabled {
			c.target(r, "disabled")
			continue
		}
		if logging.Group != 0 {
			c.reject(r, "nflogGroup", "conflict", "the firewall has one NFLOG delivery owner")
			continue
		}
		group := s.NFLogGroup
		if group == 0 {
			group = 1
		}
		if group < 1 || group > 65535 || s.Log.CopyRange < 0 || uint64(s.Log.CopyRange) > math.MaxUint32 || s.Log.DropSampleRate < 0 || uint64(s.Log.DropSampleRate) > math.MaxUint32 {
			c.reject(r, "log", "invalid_value", "invalid kernel NFLOG group, copy range or sampling divisor")
			continue
		}
		if s.Log.AcceptSampleRate != 0 {
			c.reject(r, "log.acceptSampleRate", "unsupported_semantics", "legacy accepted-packet sampling feeds DPI flow classification, which is not replaced with deny-event logging")
			continue
		}
		logging = network.PacketLog{Group: uint16(group), CopyRange: uint32(s.Log.CopyRange), Every: uint32(s.Log.DropSampleRate)}
		cfg := firewalllogconfig.Config{Group: uint16(group)}
		c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "firewall-log"), FirewallLog: &cfg})
		c.note(r, "Existing rule logs and policy deny logs retain NFLOG group, packet copy range and deny sampling. Collection writes structured packet records to the native local log owner; the old dedicated file is not recreated. The old logger ignored sinks and log.reject; native log forwarding remains independently configured. DPI classification and dedicated firewall history retention are not implied.")
	}
	return logging
}
