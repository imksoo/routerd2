package bfd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// StatusLifetime bounds current local-state observations. An Up/Init field is
// additionally capped by the original packet receive deadline, never renewed
// by redisplay, a repeated callback or a periodic publication without receive.
const StatusLifetime = 5 * time.Second

type StatusValue struct {
	Text                        string
	ObservedNanos, ExpiresNanos uint64
	Continuity                  string
	EqualityContinuity          string
}

func StatusFieldSupported(field string) bool {
	switch field {
	case "phase", "peerStates", "peers", "observedAt":
		return true
	}
	return false
}

func CurrentStatus(cfg Config, s Snapshot, field string, clock Clock) (StatusValue, bool) {
	if !StatusFieldSupported(field) || ValidateSnapshot(cfg, s) != nil || !s.Active || s.BootID != clock.BootID || clock.BoottimeNanoseconds <= 0 {
		return StatusValue{}, false
	}
	now := uint64(clock.BoottimeNanoseconds)
	if s.ObservedNanos > now || now >= s.ExpiresNanos {
		return StatusValue{}, false
	}
	cfg = Normalize(cfg)
	value := StatusValue{ObservedNanos: s.ObservedNanos, ExpiresNanos: s.ExpiresNanos}
	states := map[string]string{}
	peers := make([]string, 0, len(s.Peers))
	tokens := make([]any, 0, len(s.Peers))
	up, down := 0, 0
	for i, p := range s.Peers {
		if p.Transitions == math.MaxUint64 {
			return StatusValue{}, false
		}
		state := "Unknown"
		switch p.State {
		case StateUp:
			state = "Up"
			up++
		case StateDown, StateAdminDown:
			state = "Down"
			down++
		}
		if p.State == StateUp || p.State == StateInit {
			if p.LastReceivedBootNanoseconds > clock.BoottimeNanoseconds || clock.BoottimeNanoseconds >= p.DeadlineBootNanoseconds {
				return StatusValue{}, false
			}
			value.ObservedNanos = min(value.ObservedNanos, uint64(p.LastReceivedBootNanoseconds))
			value.ExpiresNanos = min(value.ExpiresNanos, uint64(p.DeadlineBootNanoseconds))
		}
		states[p.Address] = state
		peer := map[string]any{"address": p.Address, "localAddress": cfg.SourceAddress, "state": state, "interface": cfg.Interface, "minRx": cfg.Peers[i].MinRxMilliseconds, "minTx": cfg.Peers[i].MinTxMilliseconds, "detectMultiplier": cfg.Peers[i].DetectMultiplier}
		peers = append(peers, fmt.Sprint(peer))
		tokens = append(tokens, []any{p.Address, p.LocalDiscriminator, p.RemoteDiscriminator, p.Transitions})
	}
	phase := "Pending"
	switch {
	case down > 0 && up > 0:
		phase = "Degraded"
	case down > 0:
		phase = "Down"
	case up == len(s.Peers):
		phase = "Up"
	}
	switch field {
	case "phase":
		value.Text = phase
	case "peerStates":
		value.Text = fmt.Sprint(states)
	case "peers":
		body, _ := json.Marshal(peers)
		value.Text = string(body)
	case "observedAt":
		if s.TimestampChanges == math.MaxUint64 {
			return StatusValue{}, false
		}
		value.Text = s.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	body, _ := json.Marshal(tokens)
	sum := sha256.Sum256(body)
	value.Continuity = hex.EncodeToString(sum[:])
	if field == "observedAt" {
		body, _ = json.Marshal([]any{value.Continuity, s.TimestampChanges})
		sum = sha256.Sum256(body)
		value.EqualityContinuity = hex.EncodeToString(sum[:])
	}
	return value, true
}
