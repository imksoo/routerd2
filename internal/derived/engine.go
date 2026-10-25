package derived

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
)

var ErrCapacity = errors.New("derived state or transition capacity exhausted")

type Emission struct {
	ID    string                `json:"id"`
	Event daemonapi.DaemonEvent `json:"event"`
}
type Status struct {
	Phase             string    `json:"phase"`
	Inputs            Truth     `json:"inputs"`
	Asserted          *bool     `json:"asserted,omitempty"`
	PendingTransition string    `json:"pending_transition"`
	LastAssertedAt    time.Time `json:"last_asserted_at,omitempty"`
	LastRetractedAt   time.Time `json:"last_retracted_at,omitempty"`
}
type Result struct {
	ObservedNanos   uint64          `json:"-"`
	ValidUntilNanos uint64          `json:"-"`
	State           json.RawMessage `json:"state"`
	Emissions       []Emission      `json:"emissions"`
	Status          Status          `json:"status"`
}

type state struct {
	Digest              string `json:"digest"`
	Scope               string `json:"scope"`
	Initialized         bool   `json:"initialized"`
	InitialEmission     bool   `json:"initial_emission"`
	Asserted            bool   `json:"asserted"`
	AssertedAuthority   string `json:"asserted_authority,omitempty"`
	evaluationAuthority string
	Counter             uint64    `json:"counter"`
	LastNow             uint64    `json:"last_now"`
	LastAssertedAt      time.Time `json:"last_asserted_at,omitempty"`
	LastRetractedAt     time.Time `json:"last_retracted_at,omitempty"`
	Pending             *pending  `json:"pending,omitempty"`
}
type pending struct {
	Target            bool `json:"target"`
	Since, Due, Until uint64
	Origin            string    `json:"origin"`
	InputID           string    `json:"input_id"`
	At                time.Time `json:"at"`
}

func (p Program) load(body []byte) (state, error) {
	if p.digest == "" {
		return state{}, errors.New("derived program is not compiled")
	}
	if len(body) == 0 {
		return state{Digest: p.digest}, nil
	}
	if len(body) > 64<<10 {
		return state{}, ErrCapacity
	}
	var s state
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, err
	}
	if d.Decode(new(any)) != io.EOF {
		return s, errors.New("trailing derived state")
	}
	if s.Digest != p.digest {
		return state{Digest: p.digest}, nil
	}
	if len(s.Scope) > 1024 {
		return s, errors.New("invalid derived state scope")
	}
	if h := s.Pending; h != nil {
		if p.hold == 0 || h.Due <= h.Since || h.Due-h.Since != p.hold || h.Since > s.LastNow || h.Until <= h.Since || len(h.Origin) != 64 || !token(h.InputID, 256) || h.At.IsZero() || h.Target == s.Asserted {
			return s, errors.New("invalid derived hold state")
		}
	}
	return s, nil
}

// Advance requires a current immutable snapshot even when restoring durable
// state. Historical asserted=true never substitutes for current finite input.
// State and emitted jobs are proposals until atomically committed with a
// version fence. The caller must also fence current producer authority.
func (p Program) Advance(previous json.RawMessage, snapshot Snapshot, forceInitial bool) (Result, error) {
	if snapshot.id == "" || snapshot.scope == "" || snapshot.wall.IsZero() {
		return Result{}, errors.New("current derived observation snapshot is required")
	}
	s, err := p.load(previous)
	if err != nil {
		return Result{}, err
	}
	if s.Scope != snapshot.scope {
		s.Scope = snapshot.scope
		s.Pending = nil
	} else if snapshot.now < s.LastNow {
		return Result{}, errors.New("derived monotonic clock moved backwards")
	}
	s.LastNow = snapshot.now
	s.evaluationAuthority = snapshot.producerAuthority(p.refs)
	input := p.target(snapshot)
	first := !s.Initialized
	if first {
		s.Initialized = true
		s.InitialEmission = p.spec.EmitInitial || forceInitial
	}
	if input.truth == Indeterminate {
		s.Pending = nil
		return finish(s, nil, input.truth, input.proofUntil, input.observed)
	}
	target := input.truth == True
	var out []Emission
	emit := func(at time.Time, id string) error {
		if s.Counter == math.MaxUint64 {
			return ErrCapacity
		}
		s.Counter++
		s.Asserted = target
		s.AssertedAuthority = s.evaluationAuthority
		s.Pending = nil
		if target {
			s.LastAssertedAt = at
		} else {
			s.LastRetractedAt = at
		}
		out = append(out, p.emission(s, id, target, at))
		return nil
	}
	if first || s.InitialEmission {
		s.Asserted = target
		s.AssertedAuthority = s.evaluationAuthority
		if target {
			s.LastAssertedAt = snapshot.wall
		} else {
			s.LastRetractedAt = snapshot.wall
		}
		if s.InitialEmission {
			if err = emit(snapshot.wall, snapshot.id); err != nil {
				return Result{}, err
			}
		}
		s.InitialEmission = false
		return finish(s, out, input.truth, input.proofUntil, input.observed)
	}
	if target == s.Asserted {
		s.AssertedAuthority = s.evaluationAuthority
		s.Pending = nil
		if target && s.LastAssertedAt.IsZero() {
			s.LastAssertedAt = snapshot.wall
		}
		if !target && s.LastRetractedAt.IsZero() {
			s.LastRetractedAt = snapshot.wall
		}
		return finish(s, nil, input.truth, input.proofUntil, input.observed)
	}
	if p.hold == 0 {
		if err = emit(snapshot.wall, snapshot.id); err != nil {
			return Result{}, err
		}
		return finish(s, out, input.truth, input.proofUntil, input.observed)
	}
	origin := snapshot.authority(p.refs, p.equality)
	continuous := s.Pending != nil && snapshot.continuity != "" && snapshot.continuousSince <= s.Pending.Since
	if s.Pending == nil || s.Pending.Target != target || s.Pending.Origin != origin || snapshot.now >= s.Pending.Until && !continuous {
		if p.hold > math.MaxUint64-snapshot.now {
			return Result{}, ErrCapacity
		}
		s.Pending = &pending{Target: target, Since: snapshot.now, Due: snapshot.now + p.hold, Until: input.until, Origin: origin, InputID: snapshot.id, At: snapshot.wall.Add(time.Duration(p.hold))}
	} else {
		s.Pending.Until = input.until
		if snapshot.now >= s.Pending.Due {
			at, id := s.Pending.At, s.Pending.InputID
			if err = emit(at, id); err != nil {
				return Result{}, err
			}
		}
	}
	return finish(s, out, input.truth, input.proofUntil, input.observed)
}

// CheckpointChanged ignores the evaluation clock alone. Stable live previews
// need not fsync a duplicate state; pending deadlines and all transitions still
// require the atomic state/output commit.
func (p Program) CheckpointChanged(previous, next json.RawMessage) (bool, error) {
	a, err := p.load(previous)
	if err != nil {
		return false, err
	}
	b, err := p.load(next)
	if err != nil {
		return false, err
	}
	a.LastNow = 0
	b.LastNow = 0
	x, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(x, y), nil
}

func finish(s state, out []Emission, inputs Truth, until, observed uint64) (Result, error) {
	if s.Pending != nil {
		until = min(until, s.Pending.Due)
	}
	body, err := json.Marshal(s)
	if err != nil {
		return Result{}, err
	}
	if len(body) > 64<<10 {
		return Result{}, ErrCapacity
	}
	status := Status{Phase: "Inactive", Inputs: inputs, PendingTransition: "None", LastAssertedAt: s.LastAssertedAt, LastRetractedAt: s.LastRetractedAt}
	if inputs == Indeterminate {
		status.Phase = "Indeterminate"
	} else {
		asserted := s.Asserted
		if s.AssertedAuthority == s.evaluationAuthority {
			status.Asserted = &asserted
		}
		if s.Pending != nil {
			status.Phase = "Pending"
			status.PendingTransition = "Retracting"
			if s.Pending.Target {
				status.PendingTransition = "Asserting"
			}
		} else if asserted {
			status.Phase = "Asserted"
		} else if !s.LastRetractedAt.IsZero() {
			status.Phase = "Retracted"
		}
	}
	return Result{State: body, Emissions: out, Status: status, ValidUntilNanos: until, ObservedNanos: observed}, nil
}

func (p Program) emission(s state, input string, asserted bool, at time.Time) Emission {
	body, _ := json.Marshal([]any{p.digest, s.Scope, input, s.Counter, asserted})
	id := sha256.Sum256(body)
	suffix, reason, message := ".retracted", "DerivedEventRetracted", "derived event retracted"
	if asserted {
		suffix, reason, message = ".asserted", "DerivedEventAsserted", "derived event asserted"
	}
	return Emission{ID: hex.EncodeToString(id[:]), Event: daemonapi.DaemonEvent{TypeMeta: daemonapi.TypeMeta{APIVersion: daemonapi.APIVersion, Kind: daemonapi.KindDaemonEvent}, Time: at.UTC(), Daemon: daemonapi.DaemonRef{Name: "routerd-derived", Kind: "routerd-derived", Instance: p.name}, Resource: &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "DerivedEvent", Name: p.name}, Type: p.spec.Topic + suffix, Severity: daemonapi.SeverityInfo, Reason: reason, Message: message, Attributes: map[string]string{"asserted": strconv.FormatBool(asserted)}}}
}
