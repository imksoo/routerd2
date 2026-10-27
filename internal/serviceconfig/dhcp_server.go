package serviceconfig

import (
	"path/filepath"

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
