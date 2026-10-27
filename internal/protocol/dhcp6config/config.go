// Package dhcp6config defines DHCPv6 client intent and learned assignments.
package dhcp6config

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/peertls"
)

type Config struct {
	UseRA          bool            `json:"use_ra,omitempty"`
	Standby        bool            `json:"standby,omitempty"`
	Replication    *peertls.Config `json:"replication,omitempty"`
	Resource       string          `json:"resource"`
	Interface      string          `json:"interface"`
	StatePath      string          `json:"state_path"`
	ClientDUID     string          `json:"client_duid,omitempty"`
	IAID           uint32          `json:"iaid"`
	RequestAddress bool            `json:"request_address,omitempty"`
	RequestPrefix  bool            `json:"request_prefix,omitempty"`
	PrefixLength   int             `json:"prefix_length,omitempty"`
}

type Address struct {
	Address        string            `json:"address"`
	PreferredUntil lifetime.Deadline `json:"preferred_until"`
	ValidUntil     lifetime.Deadline `json:"valid_until"`
}
type Prefix struct {
	Prefix         string            `json:"prefix"`
	PreferredUntil lifetime.Deadline `json:"preferred_until"`
	ValidUntil     lifetime.Deadline `json:"valid_until"`
}

type SyncStatus struct {
	Epoch         uint64 `json:"epoch"`
	Revision      uint64 `json:"revision"`
	ReceivedNanos uint64 `json:"received_nanos"`
	Error         string `json:"error,omitempty"`
}

type RouterFlags struct {
	Managed     bool `json:"managed,omitempty"`
	OtherConfig bool `json:"other_config,omitempty"`
}

type Snapshot struct {
	RAStarted                 RouterFlags       `json:"ra_started,omitzero"`
	Sync                      *SyncStatus       `json:"sync,omitempty"`
	ObservedNanos             uint64            `json:"observed_nanos"`
	BootID                    string            `json:"boot_id"`
	Resource                  string            `json:"resource"`
	Interface                 string            `json:"interface"`
	IAID                      uint32            `json:"iaid"`
	ClientDUID                string            `json:"client_duid"`
	ServerDUID                string            `json:"server_duid,omitempty"`
	State                     string            `json:"state"`
	Addresses                 []Address         `json:"addresses,omitempty"`
	Prefixes                  []Prefix          `json:"prefixes,omitempty"`
	RenewAt                   lifetime.Deadline `json:"renew_at,omitempty"`
	RebindAt                  lifetime.Deadline `json:"rebind_at,omitempty"`
	DNSServers                []string          `json:"dns_servers,omitempty"`
	DomainSearch              []string          `json:"domain_search,omitempty"`
	SNTPServers               []string          `json:"sntp_servers,omitempty"`
	AFTRName                  string            `json:"aftr_name,omitempty"`
	InfoReceivedNanos         uint64            `json:"info_received_nanos,omitempty"`
	InformationRefreshSeconds uint32            `json:"information_refresh_seconds,omitempty"`
	InformationRefreshAt      lifetime.Deadline `json:"information_refresh_at,omitempty"`
}

func Validate(c Config) error {
	if c.Standby && c.Replication == nil {
		return errors.New("DHCPv6 standby requires replication")
	}
	if c.Replication != nil {
		if !c.RequestPrefix || c.ClientDUID == "" {
			return errors.New("DHCPv6 replication requires PD and an explicit shared DUID")
		}
		if err := peertls.Validate(*c.Replication); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Resource) == "" || c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\x00 \t\r\n") || !filepath.IsAbs(c.StatePath) {
		return errors.New("DHCPv6 requires a resource, valid interface and absolute state_path")
	}
	if c.PrefixLength < 0 || c.PrefixLength > 128 || !c.RequestPrefix && c.PrefixLength != 0 {
		return errors.New("DHCPv6 prefix_length requires request_prefix and must be 1..128 or zero for /60")
	}
	if c.ClientDUID != "" {
		_, err := ParseDUID(c.ClientDUID)
		return err
	}
	return nil
}
func ParseDUID(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) < 4 || len(b) > 128 {
		return nil, errors.New("client_duid must be 4..128 bytes of hexadecimal DUID")
	}
	return b, nil
}

// LeaseUntil returns the last surviving assignment budget. The bool
// distinguishes absence of assignments from an explicit infinite assignment.
func (s Snapshot) LeaseUntil() (lifetime.Deadline, bool) {
	var until lifetime.Deadline
	found := false
	add := func(d lifetime.Deadline) {
		if !found || until.Nanos != 0 && (d.Nanos == 0 || d.Nanos > until.Nanos) {
			until = d
		}
		found = true
	}
	for _, a := range s.Addresses {
		add(a.ValidUntil)
	}
	for _, p := range s.Prefixes {
		add(p.ValidUntil)
	}
	return until, found
}
func (s Snapshot) Active(clock boottime.Clock) bool {
	if s.State == "stopped" || s.State == "standby" || s.BootID != clock.BootID {
		return false
	}
	if s.State == "information" {
		_, ok := s.InformationUntil(clock)
		return ok
	}
	until, found := s.LeaseUntil()
	return found && until.Live(clock)
}
func (s Snapshot) InformationUntil(clock boottime.Clock) (lifetime.Deadline, bool) {
	if s.State == "stopped" || s.State == "standby" || s.BootID != clock.BootID || s.InfoReceivedNanos == 0 {
		return lifetime.Deadline{}, false
	}
	until := s.InformationRefreshAt
	if until.BootID != s.BootID {
		return lifetime.Deadline{}, false
	}
	if s.State != "information" {
		lease, found := s.LeaseUntil()
		if !found {
			return lifetime.Deadline{}, false
		}
		until = lifetime.Minimum(clock, until, lease)
	}
	return until, until.Live(clock)
}
func ValidateSnapshot(c Config, s Snapshot) error {
	if s.Resource != c.Resource || s.Interface != c.Interface || s.IAID != c.IAID || !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ObservedNanos > 1<<63-1 {
		return errors.New("DHCPv6 snapshot identity mismatch")
	}
	if _, err := ParseDUID(s.ClientDUID); err != nil {
		return err
	}
	switch s.State {
	case "waiting_ra", "standby", "soliciting", "requesting", "bound", "renewing", "rebinding", "declining", "information", "stopped":
	default:
		return errors.New("invalid DHCPv6 client state")
	}
	if (s.State == "stopped" || s.State == "standby") && (len(s.Addresses)+len(s.Prefixes) > 0 || s.InfoReceivedNanos != 0) {
		return errors.New("inactive DHCPv6 client retains assignments/information")
	}
	wantsAddress := c.RequestAddress && (!c.UseRA || s.RAStarted.Managed)
	if s.State == "information" && (wantsAddress || c.RequestPrefix) {
		return errors.New("DHCPv6 state does not match requested associations")
	}
	if s.State == "standby" && s.Sync == nil || s.Sync != nil && (s.State != "standby" || s.Sync.ReceivedNanos > s.ObservedNanos) {
		return errors.New("invalid DHCPv6 synchronization observation")
	}
	if s.ServerDUID != "" {
		if _, err := ParseDUID(s.ServerDUID); err != nil {
			return err
		}
	}
	if len(s.Addresses) > 64 || len(s.Prefixes) > 64 {
		return errors.New("too many DHCPv6 assignments")
	}
	if s.InfoReceivedNanos > s.ObservedNanos {
		return errors.New("DHCPv6 information is from a future observation")
	}
	for _, d := range []lifetime.Deadline{s.RenewAt, s.RebindAt, s.InformationRefreshAt} {
		if d.Validate() != nil || !d.UTC.IsZero() || d != (lifetime.Deadline{}) && d.BootID != s.BootID {
			return errors.New("DHCPv6 timers require the originating boot")
		}
	}
	seen := map[string]bool{}
	for _, a := range s.Addresses {
		ip, err := netip.ParseAddr(a.Address)
		if err != nil || !usable(ip) || a.Address != ip.String() || !wantsAddress || seen[a.Address] || !validLease(s.BootID, a.PreferredUntil, a.ValidUntil) {
			return errors.New("invalid DHCPv6 address assignment")
		}
		seen[a.Address] = true
	}
	for _, p := range s.Prefixes {
		ip, err := netip.ParsePrefix(p.Prefix)
		if err != nil || !usable(ip.Addr()) || ip != ip.Masked() || p.Prefix != ip.String() || !c.RequestPrefix || seen[p.Prefix] || !validLease(s.BootID, p.PreferredUntil, p.ValidUntil) {
			return errors.New("invalid DHCPv6 prefix assignment")
		}
		seen[p.Prefix] = true
	}
	return nil
}
func validLease(bootID string, preferred, valid lifetime.Deadline) bool {
	return preferred.Validate() == nil && valid.Validate() == nil && preferred.BootID == bootID && valid.BootID == bootID && (valid.Nanos == 0 || preferred.Nanos != 0 && preferred.Nanos <= valid.Nanos)
}
func usable(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLinkLocalUnicast()
}
