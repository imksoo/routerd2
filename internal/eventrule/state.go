package eventrule

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type state struct {
	Digest   string                  `json:"digest"`
	Keys     map[string]*correlation `json:"keys"`
	Fired    uint64                  `json:"fired"`
	Warnings uint64                  `json:"warnings"`
}
type correlation struct {
	LastSeen time.Time       `json:"last_seen"`
	Seen     map[string]bool `json:"seen,omitempty"`
	Sequence int             `json:"sequence,omitempty"`
	Window   []time.Time     `json:"window,omitempty"`
	LastEmit time.Time       `json:"last_emit,omitempty"`
	Count    int             `json:"count,omitempty"`
	Pending  *pending        `json:"pending,omitempty"`
}
type pending struct {
	Due   time.Time `json:"due"`
	Input Input     `json:"input"`
}

func (p Program) load(body []byte) (state, error) {
	if p.digest == "" {
		return state{}, errors.New("event rule is not compiled")
	}
	if len(body) == 0 {
		return state{Digest: p.digest, Keys: map[string]*correlation{}}, nil
	}
	if len(body) > maxStateBytes {
		return state{}, ErrCapacity
	}
	var s state
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, err
	}
	if d.Decode(new(any)) != io.EOF {
		return s, errors.New("trailing event rule state")
	}
	// A changed source is a new authority for observations. Timers from the
	// old spec cannot survive under its replacement.
	if s.Digest != p.digest {
		return state{Digest: p.digest, Keys: map[string]*correlation{}}, nil
	}
	if s.Keys == nil || len(s.Keys) > maxKeys {
		return s, errors.New("invalid event rule correlation state")
	}
	for key, c := range s.Keys {
		if len(key) > 4096 || c == nil || c.LastSeen.IsZero() || c.Sequence < 0 || c.Sequence >= len(p.topics) || c.Count < 0 || c.Count >= p.threshold || len(c.Seen) > len(p.topics) || len(c.Window) >= p.threshold {
			return s, errors.New("invalid bounded event rule state")
		}
		for topic := range c.Seen {
			if !p.exact(topic) {
				return s, errors.New("state contains an unknown rule input")
			}
		}
		if c.Pending != nil {
			if p.spec.Pattern.Operator != "absence" && p.spec.Pattern.Operator != "debounce" || !c.Pending.Due.Equal(c.Pending.Input.Event.Time.Add(p.delay)) {
				return s, errors.New("unexpected rule timer state")
			}
			if err := validInput(c.Pending.Input); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

func finish(s state, outputs []Emission) (Result, error) {
	if len(outputs) > maxOutputs {
		return Result{}, ErrCapacity
	}
	body, err := json.Marshal(s)
	if err != nil {
		return Result{}, err
	}
	if len(body) > maxStateBytes {
		return Result{}, ErrCapacity
	}
	return Result{State: body, Emissions: outputs}, nil
}

func (p Program) exact(topic string) bool {
	for _, candidate := range p.topics {
		if candidate == topic {
			return true
		}
	}
	return false
}
