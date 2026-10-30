// Package retention describes finite operational-event history, not live state.
package retention

import (
	"errors"
	"math"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

type Config struct {
	MaxAgeSeconds   int64                 `json:"max_age_seconds,omitempty"`
	MaxRecords      int64                 `json:"max_records,omitempty"`
	MaxBytes        int64                 `json:"max_bytes,omitempty"`
	IntervalSeconds int64                 `json:"interval_seconds,omitempty"`
	Vacuum          bool                  `json:"vacuum,omitempty"`
	Syslog          []syslogconfig.Config `json:"syslog,omitempty"`
}

func (c Config) Validate() error {
	if c.MaxAgeSeconds < 0 || c.MaxAgeSeconds > math.MaxInt64/int64(time.Second) || c.MaxRecords < 0 || c.MaxBytes < 0 || c.IntervalSeconds < 0 || c.IntervalSeconds > math.MaxInt64/int64(time.Second) {
		return errors.New("retention limits must be nonnegative and durations representable")
	}
	if c.MaxAgeSeconds == 0 && c.MaxRecords == 0 && c.MaxBytes == 0 {
		return errors.New("retention requires an age, record count or byte limit")
	}
	for _, destination := range c.Syslog {
		if err := syslogconfig.Validate(syslogconfig.Normalize(destination)); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) Interval() time.Duration {
	if c.IntervalSeconds == 0 {
		return 24 * time.Hour
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}
