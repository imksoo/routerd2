// SPDX-License-Identifier: BSD-3-Clause

// Package otlplogconfig defines log export without linking the OpenTelemetry SDK.
package otlplogconfig

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strings"
)

type Config struct {
	Endpoint         string            `json:"endpoint"`
	Insecure         bool              `json:"insecure,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	ServiceName      string            `json:"service_name,omitempty"`
	ServiceNamespace string            `json:"service_namespace,omitempty"`
	Attributes       map[string]string `json:"attributes,omitempty"`
	MinLevel         string            `json:"min_level,omitempty"`
}

func Normalize(c Config) Config {
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	if c.ServiceName == "" {
		c.ServiceName = "routerd2"
	}
	c.MinLevel = strings.ToLower(strings.TrimSpace(c.MinLevel))
	if c.MinLevel == "" {
		c.MinLevel = "info"
	}
	if c.MinLevel == "warn" {
		c.MinLevel = "warning"
	}
	c.Headers = maps.Clone(c.Headers)
	c.Attributes = maps.Clone(c.Attributes)
	return c
}

func Validate(c Config) error {
	c = Normalize(c)
	endpoint := c.Endpoint
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") {
			return errors.New("OTLP/gRPC endpoint must be an http/https URL without credentials, query or path")
		}
		endpoint = u.Host
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return errors.New("OTLP/gRPC endpoint requires a host and port")
	}
	if p, err := net.LookupPort("tcp", port); err != nil || p == 0 {
		return errors.New("invalid OTLP/gRPC port")
	}
	switch c.MinLevel {
	case "debug", "info", "warning", "error":
	default:
		return fmt.Errorf("unsupported minimum OTLP log level %q", c.MinLevel)
	}
	return nil
}
