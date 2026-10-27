package serviceconfig

import (
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
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
	HA                        *LeaseHA                       `json:"ha,omitempty"`
}

func validateDHCPHA(services []Service) error {
	byName := make(map[string]Service, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, s := range services {
		if s.DHCP6 != nil && s.DHCP6.HA != nil {
			if byName[s.DHCP6.HA.Owner].VRRP == nil {
				return fmt.Errorf("DHCPv6 %s HA requires a configured VRRP owner", s.Name)
			}
		}
		if s.DHCPServer == nil || s.DHCPServer.HA == nil {
			continue
		}
		c := s.DHCPServer
		service := byName[c.HA.Owner]
		if service.VRRP == nil {
			return fmt.Errorf("DHCP %s HA requires a configured VRRP owner", s.Name)
		}
		owner := service.VRRPConfig()
		if owner.Family != "ipv4" || owner.VIPInterface != c.Interface {
			return fmt.Errorf("DHCP %s HA owner must be the IPv4 VRRP service for its VIP interface", s.Name)
		}
		found := false
		for _, raw := range owner.Addresses {
			p, err := netip.ParsePrefix(raw)
			found = found || err == nil && p.Addr().String() == c.ServerAddress
		}
		if !found {
			return fmt.Errorf("DHCP %s server address must belong to its VRRP owner", s.Name)
		}
	}
	return nil
}

type LeaseHA struct {
	Owner       string         `json:"owner"`
	Replication peertls.Config `json:"replication"`
}

func (s Service) DHCPServerConfig(runtimeDir, stateDir string) dhcpserverconfig.Config {
	c := s.DHCPServer
	private := filepath.Join(runtimeDir, "services", s.Name)
	cfg := dhcpserverconfig.Config{
		Resource: s.Name, Interface: c.Interface, VRF: c.VRF, ServerAddress: c.ServerAddress,
		Primary: c.Primary, Scopes: c.Scopes, Reservations: c.Reservations, Authoritative: c.Authoritative,
		StickyHoldDays: c.StickyHoldDays, LogDHCP: c.LogDHCP, TrustedRelays: c.TrustedRelays,
		OfferSeconds: c.OfferSeconds, DeclineSeconds: c.DeclineSeconds, MaxLeases: c.MaxLeases,
		ProbeTimeoutMilliseconds: c.ProbeTimeoutMilliseconds, EffectTimeoutMilliseconds: c.EffectTimeoutMilliseconds,
		StatePath:  filepath.Join(stateDir, "services", s.Name, "dhcp-leases.json"),
		SocketPath: filepath.Join(private, "dhcp.sock"), StatusPath: filepath.Join(private, "status.json"),
		EventPath: filepath.Join(stateDir, "services", s.Name, "dhcp-events.jsonl"),
	}
	if c.HA != nil {
		r := c.HA.Replication
		cfg.Replication, cfg.Standby = &r, true
	}
	return dhcpserverconfig.Normalize(cfg)
}
