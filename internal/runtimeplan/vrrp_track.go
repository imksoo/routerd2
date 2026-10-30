package runtimeplan

import (
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

type vrrpTrackState struct {
	origin, continuity       string
	observed                 uint64
	lastPassed, healthyFloor int
	status                   vrrp.TrackStatus
}

func (s *vrrpTrackState) unknown(resource string) {
	s.healthyFloor = s.lastPassed
	s.status = vrrp.TrackStatus{Resource: resource, State: vrrp.TrackUnknown, Penalized: true}
}

func (r *Runtime) trackReference(resource string) derived.Reference {
	service := r.config.resources[resource].Service
	kind := "HealthCheck"
	if service.BFD != nil {
		kind = "BFD"
	}
	if service.BGP != nil {
		kind = "BGPRouter"
	}
	return derived.Reference{Kind: kind, Resource: resource, Field: "phase"}
}

// Confirmation consumes original observations, not policy RPC polls. Explicit
// condition exclusion preserves decisions/counters with the condition's budget.
func (r *Runtime) advanceVRRPTracksLocked(facts resourceprogram.FactSnapshot) error {
	refs := map[string]derived.Reference{}
	for _, cfg := range r.config.protocols.vrrp {
		for _, track := range cfg.Track {
			if len(track.IngressHealth) > 0 {
				for _, source := range track.IngressHealth {
					ref := r.trackReference(source)
					refs[ref.Key()] = ref
				}
				continue
			}
			ref := r.trackReference(track.Resource)
			refs[ref.Key()] = ref
		}
	}
	if len(refs) == 0 {
		return nil
	}
	requested := make([]derived.Reference, 0, len(refs))
	for _, ref := range refs {
		requested = append(requested, ref)
	}
	clock, values, err := r.statusValuesLocked(facts, requested)
	if err != nil {
		return err
	}
	now := uint64(clock.BoottimeNanoseconds)
	if r.vrrpTracks == nil {
		r.vrrpTracks = map[string][]vrrpTrackState{}
	}
	for name, cfg := range r.config.protocols.vrrp {
		states, exists := r.vrrpTracks[name]
		if !exists {
			states = make([]vrrpTrackState, len(cfg.Track))
			for i, track := range cfg.Track {
				states[i].unknown(track.Resource)
			}
		}
		for i, track := range cfg.Track {
			state := &states[i]
			ingress := len(track.IngressHealth) > 0
			target := r.config.resources[track.Resource]
			if !ingress && target.When != nil {
				truth, until := facts.ConditionAuthority(*target.When)
				if truth == resourceprogram.False && until > now && r.guard.bootID == clock.BootID && now < r.guard.untilNanos {
					state.status.Resource, state.status.State = track.Resource, vrrp.TrackNeutral
					state.status.ExpiresNanos = min(until, r.guard.untilNanos)
					continue
				}
			}
			var value derived.Value
			var observedThrough uint64
			if ingress {
				value, observedThrough = r.ingressTrackValue(track.IngressHealth, values)
			} else {
				value = values[r.trackReference(track.Resource).Key()]
				observedThrough = value.ObservedNanos
			}
			until := min(value.ExpiresNanos, value.ObservedNanos+uint64(track.MaxAgeMillis)*uint64(time.Millisecond))
			if !value.Known || value.Origin == "" || value.ObservedNanos > now || until <= now {
				state.unknown(track.Resource)
				continue
			}
			healthy := value.Text == "Healthy" || value.Text == "Up" || value.Text == "Established"
			var passed, failed int
			if !ingress && target.Service != nil && target.Service.Health != nil {
				sample := r.guardGrants[target.Service.ModuleName()].health
				passed, failed = sample.State.ConsecutivePassed, sample.State.ConsecutiveFailed
				if passed == 0 && failed == 0 {
					state.unknown(track.Resource)
					continue
				}
				healthy = passed > 0
			}
			if value.Origin != state.origin || value.Continuity != state.continuity || state.status.State == vrrp.TrackUnknown || now >= state.status.ExpiresNanos {
				if value.Origin == state.origin && value.Continuity == state.continuity && value.ObservedNanos <= state.observed {
					continue
				}
				*state = vrrpTrackState{origin: value.Origin, continuity: value.Continuity}
				state.unknown(track.Resource)
				state.healthyFloor = max(0, passed-1)
			}
			if !ingress && value.ObservedNanos < state.observed {
				continue
			}
			if value.ObservedNanos > state.observed {
				state.observed = observedThrough
				if !ingress && target.Service != nil && target.Service.Health != nil {
					if passed < state.lastPassed {
						state.healthyFloor = 0
					}
					state.lastPassed = passed
					state.status.HealthyCount = max(0, passed-state.healthyFloor)
					state.status.UnhealthyCount = failed
				} else if healthy {
					state.status.HealthyCount = min(track.HealthyThreshold, state.status.HealthyCount+1)
					state.status.UnhealthyCount = 0
				} else {
					state.status.UnhealthyCount = min(track.UnhealthyThreshold, state.status.UnhealthyCount+1)
					state.status.HealthyCount = 0
				}
				if state.status.UnhealthyCount >= track.UnhealthyThreshold {
					state.status.Penalized = true
				}
				if state.status.HealthyCount >= track.HealthyThreshold {
					state.status.Penalized = false
				}
			}
			state.status.Resource, state.status.ExpiresNanos = track.Resource, until
			state.status.State = vrrp.TrackUnhealthy
			if healthy {
				state.status.State = vrrp.TrackHealthy
			}
		}
		r.vrrpTracks[name] = states
	}
	return nil
}

func (r *Runtime) vrrpDecisionLocked(name string, active bool) (vrrp.Decision, error) {
	cfg, ok := r.config.protocols.vrrp[name]
	if !ok {
		return vrrp.Decision{}, ErrStale
	}
	decision := vrrp.Decision{Active: active}
	if len(cfg.Track) == 0 {
		return decision, nil
	}
	if _, err := r.conditionFactsLocked(r.config); err != nil {
		return decision, err
	}
	for _, state := range r.vrrpTracks[name] {
		decision.Track = append(decision.Track, state.status)
	}
	return decision, nil
}

// One confirmation represents a new round of backend observations: its oldest
// timestamp must pass the newest timestamp consumed last time. Polls or three
// reports from one round cannot create three confirmations. The existing Health
// workers have already applied each backend's thresholds.
func (r *Runtime) ingressTrackValue(sources []string, values map[string]derived.Value) (derived.Value, uint64) {
	var out derived.Value
	var newest uint64
	var origins []string
	healthy, unknown := true, false
	for _, source := range sources {
		value := values[r.trackReference(source).Key()]
		origins = append(origins, value.Origin)
		if !value.Known {
			unknown = true
			continue
		}
		sample := r.guardGrants[r.config.resources[source].Service.ModuleName()].health
		healthy = healthy && sample.Active
		newest = max(newest, value.ObservedNanos)
		if out.ObservedNanos == 0 || value.ObservedNanos < out.ObservedNanos {
			out.ObservedNanos = value.ObservedNanos
		}
		if out.ExpiresNanos == 0 || value.ExpiresNanos < out.ExpiresNanos {
			out.ExpiresNanos = value.ExpiresNanos
		}
	}
	out.Known = !unknown
	out.Origin = strings.Join(origins, "/")
	out.Text = "Unhealthy"
	if healthy {
		out.Text = "Healthy"
	}
	return out, newest
}
