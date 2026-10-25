package vrrp

import (
	"errors"
	"time"
)

// Track references a Health service. Its confirmation counts apply to actual
// accepted probe observations, independently of the Health service's own phase.
type Track struct {
	Service            string `json:"service"`
	Penalty            int    `json:"penalty,omitempty"`
	HealthyThreshold   int    `json:"healthy_threshold,omitempty"`
	UnhealthyThreshold int    `json:"unhealthy_threshold,omitempty"`
	MaxAgeMillis       int    `json:"max_age_millis"`
}

type TrackStatus struct {
	Service        string `json:"service"`
	Known          bool   `json:"known"`
	Penalized      bool   `json:"penalized"`
	HealthyCount   int    `json:"healthy_count"`
	UnhealthyCount int    `json:"unhealthy_count"`
	ExpiresNanos   uint64 `json:"expires_nanos,omitempty"`
}

// Decision is the completed ownership result and the current finite tracking
// policy. Priority is derived from configuration and these decisions, not a
// second mutable control-plane priority or a new instance configuration.
type Decision struct {
	Active bool          `json:"active"`
	Track  []TrackStatus `json:"track,omitempty"`
}

func initialTracks(c Config) []TrackStatus {
	var states []TrackStatus
	for _, track := range c.Track {
		states = append(states, TrackStatus{Service: track.Service, Penalized: true})
	}
	return states
}

func effectivePriority(c Config, states []TrackStatus) int {
	priority := c.Priority
	for i, state := range states {
		if state.Penalized {
			priority -= c.Track[i].Penalty
		}
	}
	return max(1, priority)
}

func validateTracks(c Config, states []TrackStatus) error {
	if len(c.Track) != len(states) {
		return errors.New("VRRP tracking decisions do not match configuration")
	}
	for i, state := range states {
		if state.Service != c.Track[i].Service || state.HealthyCount < 0 || state.UnhealthyCount < 0 || state.Known && state.ExpiresNanos == 0 || !state.Known && (!state.Penalized || state.ExpiresNanos != 0) {
			return errors.New("invalid VRRP tracking decision")
		}
	}
	return nil
}

func (m *machine) priority() int { return effectivePriority(m.config, m.track) }

func (m *machine) expireTracks(now uint64) {
	for i, state := range m.track {
		if state.Known && state.ExpiresNanos <= now {
			m.track[i] = TrackStatus{Service: state.Service, Penalized: true}
		}
	}
}

func (m *machine) retimePriority(now time.Time, previous int) {
	if m.state != StateBackup || m.priority() == previous {
		return
	}
	anchor := m.peerReceived
	if anchor.IsZero() {
		anchor = now
	}
	if m.peer.IsValid() && m.peerPriority == 0 {
		m.deadline = anchor.Add(m.skew())
	} else {
		m.deadline = anchor.Add(m.downInterval())
	}
}
