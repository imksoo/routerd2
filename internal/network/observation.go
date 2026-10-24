package network

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unicode/utf8"
)

const (
	ObservationResponseLimit   = 256 << 10
	ObservationPageLimit       = 256
	MaxDesiredObservationBytes = 2 << 20
	DesiredObservationChunk    = 64 << 10
)

var (
	ErrObservationChanged  = errors.New("network projection changed; start a new observation")
	ErrObservationNotFound = errors.New("requested network observation was not found")
	ErrObservationTooLarge = errors.New("network observation exceeds its bounded representation")
)

// ProjectionID identifies the journal intent, not an atomic kernel snapshot.
// Every page is a fresh observation: DAD, expiry, roaming and counters can change
// even when the projection ID and configuration revision remain the same.
type ObservationHeader struct {
	Revision          uint64    `json:"revision"`
	AttemptedRevision uint64    `json:"attempted_revision"`
	Status            string    `json:"status"`
	ObservedAt        time.Time `json:"observed_at"`
	ProjectionID      string    `json:"projection_id"`
}

type ObservationCounts struct {
	DesiredBytes        int `json:"desired_bytes"`
	Interfaces          int `json:"interfaces"`
	Addresses           int `json:"addresses"`
	Routes              int `json:"routes"`
	NextHops            int `json:"next_hops"`
	PolicyRules         int `json:"policy_rules"`
	WireGuard           int `json:"wireguard"`
	WireGuardPeers      int `json:"wireguard_peers"`
	WireGuardAllowedIPs int `json:"wireguard_allowed_ips"`
	ObservationErrors   int `json:"observation_errors"`
}

type ObservationTruncation struct {
	Desired             bool `json:"desired,omitempty"`
	Interfaces          bool `json:"interfaces,omitempty"`
	Addresses           bool `json:"addresses,omitempty"`
	Routes              bool `json:"routes,omitempty"`
	NextHops            bool `json:"next_hops,omitempty"`
	PolicyRules         bool `json:"policy_rules,omitempty"`
	WireGuard           bool `json:"wireguard,omitempty"`
	WireGuardPeers      bool `json:"wireguard_peers,omitempty"`
	WireGuardAllowedIPs bool `json:"wireguard_allowed_ips,omitempty"`
	LastError           bool `json:"last_error,omitempty"`
	ObservationErrors   bool `json:"observation_errors,omitempty"`
	Status              bool `json:"status,omitempty"`
	Firewall            bool `json:"firewall,omitempty"`
	VRRPGuard           bool `json:"vrrp_guard,omitempty"`
}

// ObservationView preserves all legacy Snapshot fields for small observations.
// Large responses explicitly mark each omitted/previewed section and expose its
// full count. Consumers needing a complete collection use the detail pages.
type ObservationView struct {
	Snapshot
	ObservedAt   time.Time             `json:"observed_at"`
	ProjectionID string                `json:"projection_id"`
	Complete     bool                  `json:"observation_complete"`
	Counts       ObservationCounts     `json:"counts"`
	Truncated    bool                  `json:"truncated"`
	Truncation   ObservationTruncation `json:"truncation"`
}

type ObservationQuery struct {
	Section, Interface, PublicKey, RouteID string
	Table, Offset, Limit                   int
	ProjectionID                           string
}

func (q ObservationQuery) Validate() error {
	if q.Offset < 0 || q.Limit < 0 || q.Limit > ObservationPageLimit || q.Table < 0 || uint64(q.Table) > uint64(^uint32(0)) {
		return errors.New("observation offset/table must be nonnegative and limit must be 1..256 when supplied")
	}
	if q.ProjectionID != "" && !validObservationID(q.ProjectionID) {
		return errors.New("projection_id must be a canonical SHA-256 digest")
	}
	switch q.Section {
	case "interfaces", "policy-rules", "wireguard":
		if q.Interface != "" || q.PublicKey != "" || q.RouteID != "" || q.Table != 0 {
			return errors.New("filters do not apply to this observation section")
		}
	case "routes", "route-next-hops":
		if q.Interface != "" || q.PublicKey != "" || q.Section == "routes" && q.RouteID != "" || q.Section == "route-next-hops" && !validObservationID(q.RouteID) {
			return errors.New("route-next-hops requires route_id; unrelated filters are forbidden")
		}
	case "addresses", "wireguard-peers", "wireguard-allowed-ips":
		if !validName(q.Interface) || q.Table != 0 || q.RouteID != "" {
			return errors.New("this observation section requires an interface and no route filters")
		}
		if q.Section == "wireguard-allowed-ips" {
			key, err := parseWireGuardKey(q.PublicKey)
			if err != nil || key.zero() {
				return errors.New("wireguard-allowed-ips requires a valid public_key")
			}
		} else if q.PublicKey != "" {
			return errors.New("public_key only applies to wireguard-allowed-ips")
		}
	default:
		return errors.New("unknown network observation section")
	}
	return nil
}

type ObservationPage struct {
	ObservationHeader
	Section    string            `json:"section"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	NextOffset *int              `json:"next_offset,omitempty"`
	Items      []json.RawMessage `json:"items"`
}

type DesiredPage struct {
	ObservationHeader
	TotalBytes int    `json:"total_bytes"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset,omitempty"`
	Data       []byte `json:"data_base64"`
}

type InterfaceObservation struct {
	InterfaceState
	AddressCount       int  `json:"address_count"`
	AddressesTruncated bool `json:"addresses_truncated"`
}

type RouteObservation struct {
	RouteState
	RouteID           string `json:"route_id"`
	NextHopCount      int    `json:"next_hop_count"`
	NextHopsTruncated bool   `json:"next_hops_truncated"`
}

type WireGuardObservation struct {
	WireGuardState
	PeerCount      int  `json:"peer_count"`
	PeersTruncated bool `json:"peers_truncated"`
}

type WireGuardPeerObservation struct {
	WireGuardPeerState
	AllowedIPCount      int  `json:"allowed_ip_count"`
	AllowedIPsTruncated bool `json:"allowed_ips_truncated"`
}

// SourceObservation answers one protocol dependency without loading routes,
// WireGuard peers, unrelated interfaces or the complete desired document.
type SourceObservation struct {
	ObservationHeader
	Interface        string        `json:"interface"`
	RequestedAddress string        `json:"requested_address"`
	InterfacePresent bool          `json:"interface_present"`
	Index            int           `json:"index"`
	Up               bool          `json:"up"`
	Found            bool          `json:"found"`
	Address          *AddressState `json:"address,omitempty"`
}

// ValidateSourceAddressQuery is shared by the local API and the native query;
// invalid names or addresses are rejected before any kernel observation.
func ValidateSourceAddressQuery(name, address string) error {
	_, err := validateSourceObservation(name, address)
	return err
}

func ValidateDesiredObservationQuery(projectionID string, offset, limit int) error {
	if offset < 0 || offset > MaxDesiredObservationBytes || limit < 0 || limit > DesiredObservationChunk || projectionID != "" && !validObservationID(projectionID) {
		return errors.New("invalid desired observation offset, limit or projection_id")
	}
	return nil
}

func validObservationID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func observationText(s string, maximum int) string {
	if len(s) <= maximum {
		return s
	}
	s = s[:maximum]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func observationDigest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

func snapshotHeader(s Snapshot) (ObservationHeader, error) {
	id, err := observationDigest(struct {
		Revision, AttemptedRevision uint64
		Status                      string
		Desired                     Desired
	}{s.Revision, s.AttemptedRevision, s.Status, s.Desired})
	return ObservationHeader{Revision: s.Revision, AttemptedRevision: s.AttemptedRevision, Status: observationText(s.Status, 64), ObservedAt: time.Now().UTC(), ProjectionID: id}, err
}

// BoundSnapshot supports small in-process/fake backends as well as the native
// engine. Native observation supplies its stronger pending-intent projection ID.
func BoundSnapshot(s Snapshot) (ObservationView, error) {
	h, err := snapshotHeader(s)
	if err != nil {
		return ObservationView{}, err
	}
	return boundedSnapshot(s, h)
}

func boundedSnapshot(s Snapshot, h ObservationHeader) (ObservationView, error) {
	desired, err := json.Marshal(s.Desired)
	if err != nil {
		return ObservationView{}, err
	}
	counts := ObservationCounts{DesiredBytes: len(desired), Interfaces: len(s.Interfaces), Routes: len(s.Routes), PolicyRules: len(s.PolicyRules), WireGuard: len(s.WireGuard), ObservationErrors: len(s.ObservationErrors)}
	for _, l := range s.Interfaces {
		counts.Addresses += len(l.Addresses)
	}
	for _, r := range s.Routes {
		counts.NextHops += len(r.NextHops)
	}
	for _, d := range s.WireGuard {
		counts.WireGuardPeers += len(d.Peers)
		for _, p := range d.Peers {
			counts.WireGuardAllowedIPs += len(p.AllowedIPs)
		}
	}
	v := ObservationView{Snapshot: s, ObservedAt: h.ObservedAt, ProjectionID: h.ProjectionID, Complete: len(s.ObservationErrors) == 0, Counts: counts}
	// Avoid serializing an obviously large observed data set merely to decide
	// whether it is large. The thresholds are conservative, not truncation caps.
	if counts.DesiredBytes <= ObservationResponseLimit/2 && counts.Interfaces <= 64 && counts.Addresses <= 256 && counts.Routes <= 512 && counts.NextHops <= 512 && counts.PolicyRules <= 256 && counts.WireGuardPeers <= 128 && counts.WireGuardAllowedIPs <= 512 && counts.ObservationErrors <= 16 {
		encoded, err := json.Marshal(v)
		if err != nil {
			return v, err
		}
		if len(encoded)+1 <= ObservationResponseLimit {
			return v, nil
		}
	}
	v.Truncated = true
	v.Complete = false
	v.Truncation.Status = s.Status != h.Status
	v.Status = h.Status
	v.Truncation.LastError = len(s.LastError) > 1024
	v.LastError = observationText(s.LastError, 1024)
	if len(desired) > 32<<10 {
		v.Desired = Desired{}
		v.Truncation.Desired = true
	}
	v.Interfaces = append([]InterfaceState(nil), s.Interfaces[:min(32, len(s.Interfaces))]...)
	v.Truncation.Interfaces = len(v.Interfaces) != counts.Interfaces
	shownAddresses := 0
	for i := range v.Interfaces {
		l := &v.Interfaces[i]
		l.Addresses = append([]AddressState(nil), l.Addresses[:min(8, len(l.Addresses))]...)
		shownAddresses += len(l.Addresses)
	}
	v.Truncation.Addresses = shownAddresses != counts.Addresses
	v.Routes = append([]RouteState(nil), s.Routes[:min(64, len(s.Routes))]...)
	v.Truncation.Routes = len(v.Routes) != counts.Routes
	shownHops := 0
	for i := range v.Routes {
		r := &v.Routes[i]
		r.NextHops = append([]NextHop(nil), r.NextHops[:min(8, len(r.NextHops))]...)
		shownHops += len(r.NextHops)
	}
	v.Truncation.NextHops = shownHops != counts.NextHops
	v.PolicyRules = append([]PolicyRuleState(nil), s.PolicyRules[:min(64, len(s.PolicyRules))]...)
	v.Truncation.PolicyRules = len(v.PolicyRules) != counts.PolicyRules
	v.WireGuard = append([]WireGuardState(nil), s.WireGuard[:min(16, len(s.WireGuard))]...)
	v.Truncation.WireGuard = len(v.WireGuard) != counts.WireGuard
	shownPeers, shownIPs := 0, 0
	for i := range v.WireGuard {
		d := &v.WireGuard[i]
		d.Peers = append([]WireGuardPeerState(nil), d.Peers[:min(4, len(d.Peers))]...)
		shownPeers += len(d.Peers)
		for j := range d.Peers {
			p := &d.Peers[j]
			p.AllowedIPs = append([]string(nil), p.AllowedIPs[:min(8, len(p.AllowedIPs))]...)
			shownIPs += len(p.AllowedIPs)
		}
	}
	v.Truncation.WireGuardPeers = shownPeers != counts.WireGuardPeers
	v.Truncation.WireGuardAllowedIPs = shownIPs != counts.WireGuardAllowedIPs
	v.ObservationErrors = append([]string(nil), s.ObservationErrors[:min(16, len(s.ObservationErrors))]...)
	v.Truncation.ObservationErrors = len(v.ObservationErrors) != counts.ObservationErrors
	for i, value := range v.ObservationErrors {
		if len(value) > 1024 {
			v.ObservationErrors[i] = observationText(value, 1024)
			v.Truncation.ObservationErrors = true
		}
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if len(encoded)+1 <= ObservationResponseLimit {
		return v, nil
	}
	// Unexpectedly large individual kernel attributes still cannot produce an
	// oversized response. Explicitly drop collections, preserving exact counts.
	v.Desired, v.Interfaces, v.Routes, v.PolicyRules, v.WireGuard = Desired{}, nil, nil, nil, nil
	v.Truncation.Desired = true
	v.Truncation.Interfaces, v.Truncation.Addresses = counts.Interfaces != 0, counts.Addresses != 0
	v.Truncation.Routes, v.Truncation.NextHops = counts.Routes != 0, counts.NextHops != 0
	v.Truncation.PolicyRules = counts.PolicyRules != 0
	v.Truncation.WireGuard, v.Truncation.WireGuardPeers, v.Truncation.WireGuardAllowedIPs = counts.WireGuard != 0, counts.WireGuardPeers != 0, counts.WireGuardAllowedIPs != 0
	v.Truncation.Firewall, v.Truncation.VRRPGuard = len(v.Firewall.Table) > 64, len(v.VRRPGuard.Table) > 64
	v.Firewall.Table, v.VRRPGuard.Table = observationText(v.Firewall.Table, 64), observationText(v.VRRPGuard.Table, 64)
	encoded, err = json.Marshal(v)
	if err != nil || len(encoded)+1 > ObservationResponseLimit {
		return v, ErrObservationTooLarge
	}
	return v, nil
}

func makeObservationPage(h ObservationHeader, q ObservationQuery, total int, item func(int) (any, error)) (ObservationPage, error) {
	page := ObservationPage{ObservationHeader: h, Section: q.Section, Total: total, Offset: q.Offset, Items: []json.RawMessage{}}
	limit := q.Limit
	if limit == 0 {
		limit = 64
	}
	used := 2048 // bounded header, names, offsets, separators and newline
	for i := q.Offset; i < total && len(page.Items) < limit; i++ {
		value, err := item(i)
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return page, err
		}
		if len(encoded)+2048 > ObservationResponseLimit {
			return page, fmt.Errorf("%w: %s item %d", ErrObservationTooLarge, q.Section, i)
		}
		if used+len(encoded)+1 > ObservationResponseLimit {
			break
		}
		page.Items = append(page.Items, encoded)
		used += len(encoded) + 1
	}
	if next := q.Offset + len(page.Items); next < total {
		page.NextOffset = &next
	}
	return page, nil
}

func validateSourceObservation(name, address string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(address)
	if !validName(name) || err != nil || ip.Is4In6() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
		return netip.Addr{}, errors.New("source observation requires a valid interface and numeric unicast address")
	}
	return ip, nil
}
