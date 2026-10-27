// SPDX-License-Identifier: BSD-3-Clause

// Package dnsconfig validates DNS intent without linking the worker's SQLite,
// QUIC, listener or lifecycle implementation into the PID 1 control process.
package dnsconfig

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/dnsresolver"
	mdns "github.com/miekg/dns"
)

type RuntimeConfig = dnsresolver.RuntimeConfig
type Runtime = dnsresolver.RuntimeConfig
type RuntimeZone = dnsresolver.RuntimeZone

// Config contains immutable intent and module-owned paths. Learned protocol
// observations arrive through Input; no external configuration file is reread.
type Config struct {
	Runtime    dnsresolver.RuntimeConfig `json:"runtime"`
	SocketPath string                    `json:"socket_path"`
	StatePath  string                    `json:"state_path"`
	EventPath  string                    `json:"event_path,omitempty"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func NormalizeRuntime(cfg dnsresolver.RuntimeConfig, statePath string) dnsresolver.RuntimeConfig {
	// NormalizeSpec writes listener/source fields only. Own those slices;
	// all nested values and zones remain read-only intent.
	copied := cfg
	copied.Spec.Listen = slices.Clone(cfg.Spec.Listen)
	copied.Spec.Sources = slices.Clone(cfg.Spec.Sources)
	if copied.Spec.QueryLog.Enabled && copied.Spec.QueryLog.Path == "" && statePath != "" {
		copied.Spec.QueryLog.Path = filepath.Join(filepath.Dir(statePath), "dns-queries.db")
	}
	copied.Spec = dnsresolver.NormalizeSpec(copied.Spec)
	return copied
}

func Validate(c Config) error {
	paths := map[string]bool{}
	for name, path := range map[string]string{"socket_path": c.SocketPath, "state_path": c.StatePath, "event_path": c.EventPath} {
		if path == "" && name == "event_path" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("DNS %s must be a clean absolute file path", name)
		}
		if paths[path] {
			return errors.New("DNS socket, state, event and input paths must be distinct")
		}
		paths[path] = true
	}
	return ValidateRuntimePaths(NormalizeRuntime(c.Runtime, c.StatePath), c)
}

func ValidateRuntimePaths(c dnsresolver.RuntimeConfig, paths Config) error {
	if err := ValidateRuntime(c); err != nil {
		return err
	}
	if c.Spec.QueryLog.Enabled {
		for _, path := range []string{paths.SocketPath, paths.StatePath, paths.StatePath + ".lock", paths.EventPath, paths.EventPath + ".1"} {
			for _, dbPath := range []string{c.Spec.QueryLog.Path, c.Spec.QueryLog.Path + "-wal", c.Spec.QueryLog.Path + "-shm"} {
				if dbPath == path {
					return errors.New("DNS query log and module paths must be distinct")
				}
			}
		}
	}
	return nil
}

func ValidHostname(name string) bool {
	_, ok := mdns.IsDomainName(mdns.Fqdn(name))
	return ok && name != "" && !strings.ContainsAny(name, " \t\r\n\\")
}

func ValidateRuntime(c dnsresolver.RuntimeConfig) error {
	if len(c.Spec.When.State) != 0 || len(c.Spec.When.All) != 0 || len(c.Spec.When.Any) != 0 {
		return errors.New("DNS when conditions require the resource lifecycle coordinator and are not implemented")
	}
	c = NormalizeRuntime(c, "")
	if strings.TrimSpace(c.Resource) == "" {
		return errors.New("DNS resource is required")
	}
	if err := dnsresolver.Validate(c.Spec); err != nil {
		return err
	}
	if len(c.Spec.Listen) > 128 || len(c.Spec.Sources) > 256 || len(c.Zones) > 1024 {
		return errors.New("DNS runtime exceeds listener/source/zone limits")
	}
	if c.Spec.Cache.MaxEntries < 0 || c.Spec.Cache.MaxEntries > 1000000 {
		return errors.New("DNS cache maxEntries must be 0..1000000")
	}
	for name, value := range map[string]string{"minTTL": c.Spec.Cache.MinTTL, "maxTTL": c.Spec.Cache.MaxTTL, "negativeTTL": c.Spec.Cache.NegativeTTL} {
		if value != "" {
			if d, err := time.ParseDuration(value); err != nil || d < 0 || d > time.Duration(^uint32(0))*time.Second {
				return fmt.Errorf("DNS cache %s must be a non-negative DNS lifetime", name)
			}
		}
	}
	if c.Spec.QueryLog.Enabled && (!filepath.IsAbs(c.Spec.QueryLog.Path) || filepath.Clean(c.Spec.QueryLog.Path) != c.Spec.QueryLog.Path || c.Spec.QueryLog.Path == "/") {
		return errors.New("DNS query log path must be a clean absolute file path")
	}
	zones := map[string]bool{}
	zoneNames := map[string]bool{}
	for _, zone := range c.Zones {
		name := strings.ToLower(mdns.Fqdn(firstNonEmpty(zone.Spec.Zone, zone.Name)))
		if _, ok := mdns.IsDomainName(name); !ok || zone.Name == "" || zones[zone.Name] || zoneNames[name] {
			return fmt.Errorf("DNS zone %q has an invalid or duplicate identity", zone.Name)
		}
		zones[zone.Name], zoneNames[name] = true, true
		if zone.Spec.TTL < 0 || uint64(zone.Spec.TTL) > uint64(^uint32(0)) || zone.Spec.DHCPDerived.TTL < 0 || uint64(zone.Spec.DHCPDerived.TTL) > uint64(^uint32(0)) {
			return fmt.Errorf("DNS zone %q has an invalid TTL", zone.Name)
		}
		if zone.Spec.DNSSEC.Enabled || zone.Spec.DNSSEC.Mode != "" {
			return fmt.Errorf("DNS zone %q: local DNSSEC signing/validation is not implemented; refusing to assert authenticated data without verification", zone.Name)
		}
		if zone.Spec.DHCPDerived.LeaseFile != "" || len(zone.DHCPHostRecords) != 0 {
			return fmt.Errorf("DNS zone %q accepts lease observations only through its native input", zone.Name)
		}
		for _, record := range zone.Spec.Records {
			if !ValidHostname(record.Hostname) {
				return fmt.Errorf("DNS zone %q has an invalid hostname", zone.Name)
			}
			if record.IPv4From.Resource != "" || record.IPv6From.Resource != "" {
				return fmt.Errorf("DNS zone %q has unresolved address sources", zone.Name)
			}
			if record.IPv4 != "" && net.ParseIP(record.IPv4).To4() == nil {
				return fmt.Errorf("DNS zone %q has invalid IPv4 %q", zone.Name, record.IPv4)
			}
			if record.IPv6 != "" && (net.ParseIP(record.IPv6) == nil || net.ParseIP(record.IPv6).To4() != nil) {
				return fmt.Errorf("DNS zone %q has invalid IPv6 %q", zone.Name, record.IPv6)
			}
			if record.TTL < 0 || uint64(record.TTL) > uint64(^uint32(0)) {
				return fmt.Errorf("DNS zone %q record TTL is invalid", zone.Name)
			}
		}
		for _, record := range zone.DHCPHostRecords {
			if !ValidHostname(record.Hostname) || net.ParseIP(record.IP) == nil {
				return fmt.Errorf("DNS zone %q contains an invalid recovered DHCP host", zone.Name)
			}
		}
	}
	sources := map[string]bool{}
	for _, source := range c.Spec.Sources {
		if len(source.Upstreams) > 64 || len(source.BootstrapResolver) > 64 {
			return fmt.Errorf("DNS source %q exceeds upstream limits", source.Name)
		}
		if sources[source.Name] {
			return fmt.Errorf("duplicate DNS source %q", source.Name)
		}
		sources[source.Name] = true
		for _, ref := range source.UpstreamFrom {
			if _, _, err := UpstreamProvider(ref); err != nil {
				return err
			}
		}
		if len(source.BootstrapResolverFrom) != 0 {
			return fmt.Errorf("DNS source %q has unresolved upstream references", source.Name)
		}
		for _, ref := range source.ZoneRef {
			if !zones[refName(ref)] && !zoneNames[strings.ToLower(mdns.Fqdn(ref))] {
				return fmt.Errorf("DNS source %q references missing zone %q", source.Name, ref)
			}
		}
	}
	bindings := map[string]bool{}
	for _, listener := range c.Spec.Listen {
		if len(listener.AddressFrom) != 0 {
			return fmt.Errorf("DNS listener %q has unresolved addressFrom", listener.Name)
		}
		for _, source := range listener.Sources {
			if !sources[source] {
				return fmt.Errorf("DNS listener %q references missing source %q", listener.Name, source)
			}
		}
		for _, address := range listener.Addresses {
			key := net.JoinHostPort(net.ParseIP(address).String(), fmt.Sprint(listener.Port))
			if bindings[key] {
				return fmt.Errorf("duplicate DNS listener binding %q", key)
			}
			bindings[key] = true
		}
	}
	return nil
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
