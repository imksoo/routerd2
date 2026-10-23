package serviceconfig

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

// DHCPServer exposes protocol intent; the runtime owns journal and API paths.
type DHCPServer struct {
	Interface                 string                         `json:"interface"`
	VRF                       string                         `json:"vrf,omitempty"`
	ServerAddress             string                         `json:"server_address"`
	Primary                   dhcpserverconfig.Scope         `json:"primary"`
	Scopes                    []dhcpserverconfig.Scope       `json:"scopes,omitempty"`
	Reservations              []dhcpserverconfig.Reservation `json:"reservations,omitempty"`
	Authoritative             bool                           `json:"authoritative,omitempty"`
	StickyHoldDays            int                            `json:"sticky_hold_days,omitempty"`
	LogDHCP                   bool                           `json:"log_dhcp,omitempty"`
	TrustedRelays             []string                       `json:"trusted_relays,omitempty"`
	OfferSeconds              int                            `json:"offer_seconds,omitempty"`
	DeclineSeconds            int                            `json:"decline_seconds,omitempty"`
	MaxLeases                 int                            `json:"max_leases,omitempty"`
	ProbeTimeoutMilliseconds  int                            `json:"probe_timeout_milliseconds,omitempty"`
	EffectTimeoutMilliseconds int                            `json:"effect_timeout_milliseconds,omitempty"`
}

func (s Service) DHCPServerConfig(runtimeDir, stateDir string) dhcpserverconfig.Config {
	c := s.DHCPServer
	private := filepath.Join(runtimeDir, "services", s.Name)
	return dhcpserverconfig.Normalize(dhcpserverconfig.Config{
		Resource: s.Name, Interface: c.Interface, VRF: c.VRF, ServerAddress: c.ServerAddress,
		Primary: c.Primary, Scopes: c.Scopes, Reservations: c.Reservations, Authoritative: c.Authoritative,
		StickyHoldDays: c.StickyHoldDays, LogDHCP: c.LogDHCP, TrustedRelays: c.TrustedRelays,
		OfferSeconds: c.OfferSeconds, DeclineSeconds: c.DeclineSeconds, MaxLeases: c.MaxLeases,
		ProbeTimeoutMilliseconds: c.ProbeTimeoutMilliseconds, EffectTimeoutMilliseconds: c.EffectTimeoutMilliseconds,
		StatePath:  filepath.Join(stateDir, "services", s.Name, "dhcp-leases.json"),
		SocketPath: filepath.Join(private, "dhcp.sock"), StatusPath: filepath.Join(private, "status.json"),
		EventPath: filepath.Join(stateDir, "services", s.Name, "dhcp-events.jsonl"),
	})
}

func (s Service) HasDNSLeaseSources() bool {
	if s.DNS == nil {
		return false
	}
	for _, zone := range s.DNS.Zones {
		if len(zone.Spec.DHCPDerived.Sources) != 0 {
			return true
		}
	}
	return false
}

func validateDNSLeaseSources(services []Service) error {
	servers := map[string]dhcpserverconfig.Config{}
	for _, s := range services {
		if s.DHCPServer != nil {
			servers[s.Name] = s.DHCPServerConfig("/run/routerd", "/var/lib/routerd")
		}
	}
	for _, s := range services {
		if s.DNS == nil {
			continue
		}
		sets, records := 0, 0
		for _, zone := range s.DNS.Zones {
			seen := map[string]bool{}
			for _, ref := range zone.Spec.DHCPDerived.Sources {
				name, valid := dnsconfig.LeaseSourceName(ref)
				server, found := servers[name]
				if !valid || !found || seen[name] || len(zone.Name) > 256 {
					return fmt.Errorf("DNS service %s zone %s requires distinct configured DHCP server sources; unresolved reference %q", s.Name, zone.Name, ref)
				}
				seen[name] = true
				sets++
				records += server.MaxLeases
			}
		}
		if sets > 128 || records > dnsconfig.MaxLeaseRecords {
			return errors.New("DNS lease sources exceed 128 source sets or 4096 possible records; reduce max_leases or split DNS instances")
		}
	}
	return nil
}
