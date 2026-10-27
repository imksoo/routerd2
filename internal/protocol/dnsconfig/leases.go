// SPDX-License-Identifier: BSD-3-Clause

package dnsconfig

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

const MaxLeaseRecords = 4096

// Input is a complete, source-scoped view of DHCP bindings and learned DNS.
// It is delivered separately from immutable DNS configuration, so renewing a
// lease does not replace the listener or upstream pools. ConfigDigest fences an
// old worker from consuming a different committed DNS instance's projection.
type Input struct {
	Resource     string            `json:"resource"`
	ConfigDigest string            `json:"config_digest"`
	Sets         []LeaseSet        `json:"sets"`
	Upstreams    []LearnedUpstream `json:"upstreams"`
	Bootstraps   []LearnedUpstream `json:"bootstraps"`
}

type LearnedUpstream struct {
	Source     string            `json:"source"`
	Provider   string            `json:"provider"`
	Address    netip.Addr        `json:"address"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type LeaseSet struct {
	Zone          string        `json:"zone"`
	Source        string        `json:"source"`
	Epoch         string        `json:"epoch"`
	Sequence      uint64        `json:"sequence"`
	ObservedAt    time.Time     `json:"observed_at"`
	BootID        string        `json:"boot_id"`
	ObservedNanos uint64        `json:"observed_nanos"`
	ExpiresNanos  uint64        `json:"expires_nanos"`
	Records       []LeaseRecord `json:"records"`
}

type LeaseRecord struct {
	Hostname  string    `json:"hostname"`
	Address   string    `json:"address"`
	ExpiresAt time.Time `json:"expires_at"`
}

func LeaseSourceName(ref string) (string, bool) {
	name := strings.TrimPrefix(ref, "DHCPv4Server/")
	if name == "" || len(name) > 24 || name[0] < 'a' || name[0] > 'z' {
		return "", false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "", false
		}
	}
	return name, true
}

func ValidLeaseHostname(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// NormalizeInput copies, canonicalizes and sorts the snapshot. It
// never repairs an invalid identity or deadline; validation still rejects it.
func NormalizeInput(p Input) Input {
	out := p
	out.Upstreams = slices.Clone(p.Upstreams)
	out.Bootstraps = slices.Clone(p.Bootstraps)
	out.Sets = slices.Clone(p.Sets)
	for i := range out.Sets {
		s := &out.Sets[i]
		s.Records = slices.Clone(s.Records)
		if name, ok := LeaseSourceName(s.Source); ok {
			s.Source = name
		}
		for j := range s.Records {
			r := &s.Records[j]
			r.Hostname = strings.ToLower(strings.TrimSuffix(r.Hostname, "."))
			if a, err := netip.ParseAddr(r.Address); err == nil {
				r.Address = a.String()
			}
		}
		sort.Slice(s.Records, func(i, j int) bool {
			if s.Records[i].Hostname != s.Records[j].Hostname {
				return s.Records[i].Hostname < s.Records[j].Hostname
			}
			return s.Records[i].Address < s.Records[j].Address
		})
	}
	sort.Slice(out.Sets, func(i, j int) bool {
		if out.Sets[i].Zone != out.Sets[j].Zone {
			return out.Sets[i].Zone < out.Sets[j].Zone
		}
		return out.Sets[i].Source < out.Sets[j].Source
	})
	return out
}

func ValidateInput(c RuntimeConfig, p Input, clock boottime.Clock) error {
	if p.Resource != c.Resource || len(p.Sets) > 128 {
		return errors.New("DNS lease projection has an invalid resource identity or exceeds 128 source sets")
	}
	if len(p.ConfigDigest) != 64 {
		return errors.New("DNS lease projection requires the instance configuration digest")
	}
	if _, err := hex.DecodeString(p.ConfigDigest); err != nil {
		return errors.New("invalid DNS lease configuration digest")
	}
	allowed := map[string]bool{}
	for _, zone := range c.Zones {
		for _, ref := range zone.Spec.DHCPDerived.Sources {
			if name, ok := LeaseSourceName(ref); ok {
				allowed[zone.Name+"/"+name] = true
			}
		}
	}
	seen, count := map[string]bool{}, 0
	for _, s := range p.Sets {
		source, ok := LeaseSourceName(s.Source)
		key := s.Zone + "/" + source
		if !ok || len(s.Zone) > 256 || !allowed[key] || seen[key] {
			return fmt.Errorf("DNS lease source is duplicate or not authorized for zone %q", s.Zone)
		}
		seen[key] = true
		if len(s.Epoch) != 32 || s.Sequence == 0 {
			return errors.New("DNS lease snapshot requires a producer epoch and sequence")
		}
		if _, err := hex.DecodeString(s.Epoch); err != nil {
			return errors.New("DNS lease snapshot epoch is invalid")
		}
		if s.ObservedAt.IsZero() || s.BootID != clock.BootID || s.ObservedNanos == 0 || s.ObservedNanos > uint64(clock.BoottimeNanoseconds) || s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos-s.ObservedNanos > uint64(dhcpserverconfig.ObservationLifetime) {
			return errors.New("DNS lease source requires its original boot-scoped observation and deadline")
		}
		addresses := map[string]bool{}
		for _, r := range s.Records {
			count++
			if count > MaxLeaseRecords {
				return errors.New("DNS lease projection exceeds 4096 records")
			}
			a, err := netip.ParseAddr(r.Address)
			if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() || a.String() == "255.255.255.255" || addresses[a.String()] {
				return errors.New("DNS lease source requires unique unicast IPv4 addresses")
			}
			addresses[a.String()] = true
			if !ValidLeaseHostname(r.Hostname) || r.ExpiresAt.IsZero() || r.ExpiresAt.Year() < 1970 || r.ExpiresAt.Year() > 9999 {
				return errors.New("DNS lease record requires a valid hostname and finite expiry")
			}
		}
	}
	if err := validateUpstreams(c, p, clock); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > 1792<<10 {
		return errors.New("DNS lease projection exceeds its 1792 KiB IPC budget")
	}
	return nil
}
