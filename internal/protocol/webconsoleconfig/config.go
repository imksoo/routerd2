// SPDX-License-Identifier: BSD-3-Clause

// Package webconsoleconfig defines the read-only operator console listener.
package webconsoleconfig

import (
	"errors"
	"net/netip"
	"net/url"
	"path"
	"strings"
)

type Config struct {
	Listen   netip.AddrPort `json:"listen"`
	BasePath string         `json:"base_path,omitempty"`
	Title    string         `json:"title,omitempty"`
	Links    []Link         `json:"links,omitempty"`
}

type Link struct {
	Label       string `json:"label"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

func Normalize(c Config) Config {
	if c.BasePath == "" {
		c.BasePath = "/"
	}
	if c.BasePath != "/" && !strings.HasSuffix(c.BasePath, "/") {
		c.BasePath += "/"
	}
	if c.Title == "" {
		c.Title = "routerd2"
	}
	return c
}

func Validate(c Config) error {
	c = Normalize(c)
	if !c.Listen.IsValid() || c.Listen.Port() == 0 || c.Listen.Addr().IsMulticast() || c.Listen.Addr().Is4In6() {
		return errors.New("web console requires a unicast or wildcard IP listener and nonzero port")
	}
	if !strings.HasPrefix(c.BasePath, "/") || c.BasePath != "/" && (path.Clean(c.BasePath)+"/" != c.BasePath) || strings.ContainsAny(c.BasePath, "?#%\\") {
		return errors.New("web console base path must be a clean absolute path ending in /")
	}
	for _, link := range c.Links {
		u, err := url.Parse(link.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" || strings.TrimSpace(link.Label) == "" {
			return errors.New("web console links require a label and an HTTP(S) URL without credentials")
		}
	}
	return nil
}
