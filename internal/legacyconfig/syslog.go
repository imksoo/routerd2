// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// LogSink selection and defaults retain routerd pkg/eventlog.NewSelected/NewSink.
// Delivery runs separately from the local log owner so a receiver outage cannot
// block local recording or network application.
func (c *compiler) logSink(r resource) {
	c.allow(r, "type", "enabled", "minLevel", "syslog")
	s := r.Spec.(api.LogSinkSpec)
	if s.Type != "syslog" {
		c.reject(r, "type", "unsupported_semantics", "only UDP syslog delivery is implemented; other log destinations are not discarded")
		return
	}
	if s.Enabled != nil && !*s.Enabled {
		c.target(r, "disabled")
		c.note(r, "Disabled LogSink retains no running delivery service, as in routerd eventlog.NewSelected.")
		return
	}
	// Omission selects the local Unix syslog in routerd, not UDP.
	if s.Syslog.Network != "udp" {
		c.reject(r, "syslog.network", "unsupported_semantics", "UDP syslog requires network: udp; local Unix and TCP delivery are not converted to UDP")
		return
	}
	tag := s.Syslog.Tag
	if tag == "" {
		tag = "routerd"
	}
	cfg := syslogconfig.Normalize(syslogconfig.Config{
		Address: s.Syslog.Address, Facility: s.Syslog.Facility,
		Tag: tag, MinLevel: s.MinLevel,
	})
	if err := syslogconfig.Validate(cfg); err != nil {
		c.reject(r, "syslog", "invalid_value", "invalid UDP syslog destination, facility, tag or minimum level")
		return
	}
	name := c.serviceName(r, "syslog")
	c.addService(r, serviceconfig.Service{Name: name, Syslog: &cfg})
	c.note(r, "UDP syslog retains address, facility, tag and minimum severity. An independent delivery worker reads the native local log stream; receiver failure does not block local recording. No external syslog daemon is introduced.")
}
