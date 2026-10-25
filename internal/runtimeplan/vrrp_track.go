package runtimeplan

import (
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type trackSource struct {
	digest, epoch, boot string
	generation          uint64
}

type vrrpTrackState struct {
	source       trackSource
	observed     uint64
	lastPassed   int
	healthyFloor int
	status       vrrp.TrackStatus
}

func (s *vrrpTrackState) unknown(service string) {
	s.healthyFloor = s.lastPassed
	s.status = vrrp.TrackStatus{Service: service, Penalized: true}
}

// Advancing an accepted Health probe, rather than an RPC poll, owns hysteresis.
// Raw consecutive counters also preserve progress through coalesced delivery.
// An authority gap starts a new confirmation streak, never a synthetic failure.
func (r *Runtime) advanceVRRPTracksLocked(u serviceconfig.Update, sample health.Snapshot) {
	for name, cfg := range r.config.vrrp {
		if len(cfg.Track) == 0 {
			continue
		}
		if r.vrrpTracks == nil {
			r.vrrpTracks = make(map[string][]vrrpTrackState)
		}
		states, exists := r.vrrpTracks[name]
		if !exists {
			states = make([]vrrpTrackState, len(cfg.Track))
			for i, track := range cfg.Track {
				states[i].unknown(track.Service)
			}
			r.vrrpTracks[name] = states
		}
		for i, track := range cfg.Track {
			if "service."+track.Service != u.Name {
				continue
			}
			state := &states[i]
			source := trackSource{u.ConfigDigest, u.ProducerEpoch, sample.BootID, u.Generation}
			if source == state.source && sample.ObservedNanos <= state.observed {
				continue // retransmission or clock rewind is not a new probe
			}
			if source != state.source || !state.status.Known || sample.ObservedNanos >= state.status.ExpiresNanos {
				*state = vrrpTrackState{source: source}
				state.unknown(track.Service)
				state.healthyFloor = max(0, sample.State.ConsecutivePassed-1)
			}
			if sample.State.ConsecutivePassed < state.lastPassed {
				state.healthyFloor = 0
			}
			state.observed, state.lastPassed = sample.ObservedNanos, sample.State.ConsecutivePassed
			if sample.Stopped {
				state.unknown(track.Service)
				continue
			}
			passed := max(0, sample.State.ConsecutivePassed-state.healthyFloor)
			if sample.State.ConsecutiveFailed >= track.UnhealthyThreshold {
				state.status.Penalized = true
			} else if passed >= track.HealthyThreshold {
				state.status.Penalized = false
			}
			state.status.Service, state.status.Known = track.Service, true
			state.status.HealthyCount, state.status.UnhealthyCount = passed, sample.State.ConsecutiveFailed
			state.status.ExpiresNanos = min(sample.ExpiresNanos, sample.ObservedNanos+uint64(track.MaxAgeMillis)*uint64(time.Millisecond))
		}
	}
}

// The coordinator holds mu after completing kernel effects. Registry loss,
// stopped input and original source expiry all impose the configured penalty.
func (r *Runtime) vrrpDecisionLocked(name string, active bool) (vrrp.Decision, error) {
	cfg, ok := r.config.vrrp[name]
	if !ok {
		return vrrp.Decision{}, ErrStale
	}
	decision := vrrp.Decision{Active: active}
	if len(cfg.Track) == 0 {
		return decision, nil
	}
	clock, err := boottime.Read()
	if err != nil {
		return decision, err
	}
	now := uint64(clock.BoottimeNanoseconds)
	for i, track := range cfg.Track {
		status := vrrp.TrackStatus{Service: track.Service, Penalized: true}
		if states, exists := r.vrrpTracks[name]; exists {
			state := &states[i]
			sourceName := "service." + track.Service
			binding, bound := r.bindings[sourceName]
			module, registered := r.guardStatus[sourceName]
			grant, granted := r.guardGrants[sourceName]
			current := r.records[sourceName]
			known := bound && registered && granted && module.Role == "health" && module.Ready && module.Generation == current.Generation && module.ConfigDigest == binding.digest && current.ConfigDigest == binding.digest && grant.generation == current.Generation && grant.epoch == current.ProducerEpoch && grant.sequence == current.Sequence && grant.health != nil && !grant.health.Stopped && clock.BootID == state.source.boot && clock.BootID == r.guardBootID && now < r.guardUntilNanos && now < state.status.ExpiresNanos
			if !known {
				state.unknown(track.Service)
			}
			status = state.status
		}
		decision.Track = append(decision.Track, status)
	}
	return decision, nil
}
