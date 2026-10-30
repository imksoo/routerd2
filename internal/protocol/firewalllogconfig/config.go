// SPDX-License-Identifier: BSD-3-Clause

// Package firewalllogconfig describes an NFLOG reader without linking it to boot.
package firewalllogconfig

import (
	"errors"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/retention"
)

type Config struct {
	Group     uint16            `json:"group"`
	Retention *retention.Config `json:"retention,omitempty"`
	// HistoryPath is assigned by the service compiler below its state directory.
	HistoryPath string `json:"history_path,omitempty"`
}

func Validate(c Config) error {
	if c.Group == 0 {
		return errors.New("firewall log group must be 1..65535")
	}
	if c.HistoryPath != "" && (!filepath.IsAbs(c.HistoryPath) || filepath.Clean(c.HistoryPath) != c.HistoryPath || c.HistoryPath == "/") {
		return errors.New("firewall history requires a clean absolute file path")
	}
	if c.Retention != nil {
		if err := c.Retention.Validate(); err != nil {
			return err
		}
		if c.Retention.MaxAgeSeconds == 0 || c.Retention.MaxRecords != 0 || c.Retention.MaxBytes != 0 {
			return errors.New("firewall history retention supports an age limit")
		}
	}
	return nil
}
