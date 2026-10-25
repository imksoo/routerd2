package bgpconfig

import (
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

const ObservationLifetime = 5 * time.Second

type StatusValue struct {
	Text                        string
	ObservedNanos, ExpiresNanos uint64
}

// StampObservation is called before the engine walk, never after slow effects.
func StampObservation(s *Snapshot, clock bfd.Clock) {
	s.BootID, s.ObservedNanos, s.ObservedAt = clock.BootID, uint64(clock.BoottimeNanoseconds), clock.WallTime.UTC()
	s.ExpiresNanos, s.ValidUntil = 0, time.Time{}
	if s.Active {
		s.ExpiresNanos = s.ObservedNanos + uint64(ObservationLifetime)
		s.ValidUntil = s.ObservedAt.Add(ObservationLifetime)
	}
}

func ValidateObservation(s Snapshot) error {
	if !bfdBootID.MatchString(s.BootID) || s.ObservedNanos == 0 || s.ObservedNanos > math.MaxInt64 || s.ObservedAt.IsZero() || s.ObservedAt.Year() < 1970 || s.ObservedAt.Year() > 9999 {
		return errors.New("invalid BGP observation clock")
	}
	if s.Active {
		if s.ExpiresNanos != s.ObservedNanos+uint64(ObservationLifetime) || !s.ValidUntil.Equal(s.ObservedAt.Add(ObservationLifetime)) {
			return errors.New("invalid BGP observation deadline")
		}
	} else if s.ExpiresNanos != 0 || !s.ValidUntil.IsZero() || len(s.Routes) > 0 {
		return errors.New("inactive BGP retained ownership")
	}
	return nil
}

func CurrentObservation(s Snapshot, clock bfd.Clock) bool {
	return ValidateObservation(s) == nil && s.Active && s.BootID == clock.BootID && clock.BoottimeNanoseconds > 0 && s.ObservedNanos <= uint64(clock.BoottimeNanoseconds) && uint64(clock.BoottimeNanoseconds) < s.ExpiresNanos
}

func ValidatePeers(cfg Config, s Snapshot) error {
	if !s.Active {
		return nil
	}
	if len(s.Peers) != len(cfg.Runtime.Peers) {
		return errors.New("incomplete BGP peer observation")
	}
	seen := map[string]bool{}
	for _, p := range s.Peers {
		intent, exists := cfg.Runtime.Peers[p.Address]
		if !exists || seen[p.Address] || p.ASN != intent.ASN {
			return errors.New("BGP peer observation differs from committed identity")
		}
		seen[p.Address] = true
		switch p.State {
		case "SESSION_STATE_UNSPECIFIED", "SESSION_STATE_IDLE", "SESSION_STATE_CONNECT", "SESSION_STATE_ACTIVE", "SESSION_STATE_OPENSENT", "SESSION_STATE_OPENCONFIRM", "SESSION_STATE_ESTABLISHED":
		default:
			return errors.New("invalid BGP peer state")
		}
		switch p.AdminState {
		case "ADMIN_STATE_UNSPECIFIED", "ADMIN_STATE_UP", "ADMIN_STATE_DOWN", "ADMIN_STATE_PFX_CT":
		default:
			return errors.New("invalid BGP administrative state")
		}
	}
	return nil
}

func StatusFieldSupported(field string) bool {
	switch field {
	case "phase", "backend", "establishedPeers", "observedAt":
		return true
	}
	return false
}

// unresolved<0 means no matching successfully applied network projection yet.
func CurrentStatus(cfg Config, s Snapshot, field string, clock bfd.Clock, unresolved int) (StatusValue, bool) {
	if !StatusFieldSupported(field) || unresolved < 0 || s.Resource != cfg.Resource || !bfdEpoch.MatchString(s.Epoch) || s.Sequence == 0 || !CurrentObservation(s, clock) || ValidatePeers(cfg, s) != nil || s.Error != "" {
		return StatusValue{}, false
	}
	established := 0
	for _, p := range s.Peers {
		if p.State == "SESSION_STATE_ESTABLISHED" {
			established++
		}
	}
	text := ""
	switch field {
	case "phase":
		switch {
		case len(s.Peers) == 0:
			text = "Pending"
		case established == len(s.Peers) && unresolved == 0:
			text = "Established"
		case established > 0:
			text = "Degraded"
		default:
			text = "Down"
		}
	case "establishedPeers":
		text = strconv.Itoa(established)
	case "backend":
		text = "gobgp"
	case "observedAt":
		text = s.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return StatusValue{Text: text, ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ExpiresNanos}, true
}
