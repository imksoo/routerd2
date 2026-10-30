// SPDX-License-Identifier: BSD-3-Clause

// Package trafficflowconfig describes read-only kernel connection sampling.
package trafficflowconfig

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/imksoo/routerd2/internal/retention"
)

type Config struct {
	// PacketGroup supplies copied packets to the single classifier owner.
	ApplicationLayer bool              `json:"application_layer,omitempty"`
	PacketGroup      uint16            `json:"packet_group,omitempty"`
	IntervalSeconds  int64             `json:"interval_seconds,omitempty"`
	Retention        *retention.Config `json:"retention,omitempty"`
	// HistoryPath belongs to the service compiler, not operator configuration.
	HistoryPath string `json:"history_path,omitempty"`
}

func (c Config) Interval() time.Duration {
	if c.IntervalSeconds == 0 {
		return 30 * time.Second
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

func Validate(c Config) error {
	if c.ApplicationLayer && c.PacketGroup == 0 {
		return errors.New("application classification requires a packet group")
	}
	if c.IntervalSeconds < 0 || c.IntervalSeconds > int64((1<<63-1)/time.Second) {
		return errors.New("traffic flow sampling requires a positive interval")
	}
	if c.HistoryPath != "" && (!filepath.IsAbs(c.HistoryPath) || filepath.Clean(c.HistoryPath) != c.HistoryPath || c.HistoryPath == "/") {
		return errors.New("traffic flow history requires a clean absolute file path")
	}
	if c.Retention != nil {
		if err := c.Retention.Validate(); err != nil {
			return err
		}
		if c.Retention.MaxAgeSeconds == 0 || c.Retention.MaxRecords != 0 || c.Retention.MaxBytes != 0 {
			return errors.New("traffic flow retention supports an age limit")
		}
	}
	return nil
}
