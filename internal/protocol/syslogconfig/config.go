// SPDX-License-Identifier: BSD-3-Clause

// Package syslogconfig defines UDP log forwarding without linking the worker.
package syslogconfig

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

type Config struct {
	Address  string `json:"address"`
	Facility string `json:"facility,omitempty"`
	Tag      string `json:"tag,omitempty"`
	MinLevel string `json:"min_level,omitempty"`
}

func Normalize(c Config) Config {
	c.Address = strings.TrimSpace(c.Address)
	c.Facility = strings.ToLower(strings.TrimSpace(c.Facility))
	if c.Facility == "" {
		c.Facility = "local6"
	}
	if c.Tag == "" {
		c.Tag = "routerd2"
	}
	c.MinLevel = strings.ToLower(strings.TrimSpace(c.MinLevel))
	if c.MinLevel == "" {
		c.MinLevel = "info"
	}
	if c.MinLevel == "warn" {
		c.MinLevel = "warning"
	}
	return c
}

func Validate(c Config) error {
	c = Normalize(c)
	host, port, err := net.SplitHostPort(c.Address)
	if err != nil {
		return fmt.Errorf("syslog address: %w", err)
	}
	if host == "" {
		return errors.New("syslog destination host is required")
	}
	if p, err := net.LookupPort("udp", port); err != nil || p == 0 {
		return fmt.Errorf("invalid syslog UDP port %q", port)
	}
	if _, ok := facilities[c.Facility]; !ok {
		return fmt.Errorf("unsupported syslog facility %q", c.Facility)
	}
	switch c.MinLevel {
	case "debug", "info", "warning", "error":
	default:
		return fmt.Errorf("unsupported minimum syslog level %q", c.MinLevel)
	}
	if strings.ContainsAny(c.Tag, "\x00\r\n") {
		return errors.New("syslog tag must not contain NUL or line breaks")
	}
	return nil
}

// FacilityPriority returns the facility bits after Normalize and Validate.
func (c Config) FacilityPriority() int { return facilities[c.Facility] << 3 }

// These are the facilities supported by routerd's eventlog.SyslogSink.
var facilities = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4,
	"syslog": 5, "lpr": 6, "news": 7, "uucp": 8, "cron": 9,
	"authpriv": 10, "ftp": 11,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}
