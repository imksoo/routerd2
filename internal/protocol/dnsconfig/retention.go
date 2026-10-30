// SPDX-License-Identifier: BSD-3-Clause
package dnsconfig

import (
	"errors"

	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
	"github.com/imksoo/routerd2/internal/retention"
)

// QueryRetention belongs to the resolver that writes its query database.
type QueryRetention struct {
	MaxAgeSeconds   int64                 `json:"max_age_seconds"`
	IntervalSeconds int64                 `json:"interval_seconds,omitempty"`
	Vacuum          bool                  `json:"vacuum,omitempty"`
	Syslog          []syslogconfig.Config `json:"syslog,omitempty"`
}

func (p QueryRetention) Validate() error {
	if p.MaxAgeSeconds < 1 {
		return errors.New("DNS query retention requires a positive age")
	}
	return (retention.Config{MaxAgeSeconds: p.MaxAgeSeconds, IntervalSeconds: p.IntervalSeconds, Syslog: p.Syslog}).Validate()
}
