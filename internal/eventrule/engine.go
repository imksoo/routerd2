package eventrule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bus"
	"github.com/imksoo/routerd/pkg/daemonapi"
)

func validInput(in Input) error {
	if !token(in.ID, 256) || !token(in.Event.Type, 128) || in.Event.Time.IsZero() || len(in.Event.Attributes) > 64 || !in.ExpiresAt.IsZero() && in.ExpiresAt.Before(in.Event.Time) {
		return errors.New("invalid event rule input envelope")
	}
	for _, s := range []string{in.Event.APIVersion, in.Event.Kind, in.Event.Cursor, in.Event.Daemon.Name, in.Event.Daemon.Kind, in.Event.Daemon.Instance, in.Event.Severity, in.Event.Reason, in.Event.Message} {
		if len(s) > 4096 || !utf8.ValidString(s) {
			return errors.New("invalid event rule input metadata")
		}
	}
	if in.Event.Resource != nil {
		for _, s := range []string{in.Event.Resource.APIVersion, in.Event.Resource.Kind, in.Event.Resource.Name} {
			if len(s) > 128 || !utf8.ValidString(s) {
				return errors.New("invalid event rule resource reference")
			}
		}
	}
	for k, v := range in.Event.Attributes {
		if !token(k, 128) || len(v) > 4096 || !utf8.ValidString(v) {
			return errors.New("invalid event rule input attributes")
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if len(b) > 32<<10 {
		return ErrCapacity
	}
	return nil
}

func (p Program) matches(topic string) bool {
	for _, pattern := range p.topics {
		if bus.MatchTopic(pattern, topic) {
			return true
		}
	}
	return false
}

func (p Program) correlation(in Input) (string, bool) {
	path := p.spec.Pattern.CorrelateBy
	if path == "" {
		return "default", true
	}
	var value string
	if strings.HasPrefix(path, "attributes.") {
		value = in.Event.Attributes[strings.TrimPrefix(path, "attributes.")]
	} else {
		switch path {
		case "resource.name":
			if in.Event.Resource != nil {
				value = in.Event.Resource.Name
			}
		case "resource.kind":
			if in.Event.Resource != nil {
				value = in.Event.Resource.Kind
			}
		case "resource.apiVersion":
			if in.Event.Resource != nil {
				value = in.Event.Resource.APIVersion
			}
		case "daemon.instance":
			value = in.Event.Daemon.Instance
		case "daemon.kind":
			value = in.Event.Daemon.Kind
		}
	}
	return value, value != "" || p.spec.Pattern.AllowMissingCorrelation
}

// Advance has no external effects. The consumer must atomically commit State
// and Emissions with the exact input cursor. A retry from the same checkpoint
// produces stable identities. Event expiry is a delivery bound, not a lease.
func (p Program) Advance(previous json.RawMessage, in Input, now time.Time) (Result, error) {
	if err := validInput(in); err != nil {
		return Result{}, err
	}
	if now.IsZero() {
		return Result{}, errors.New("event rule processing time is required")
	}
	s, err := p.load(previous)
	if err != nil {
		return Result{}, err
	}
	if in.Event.Daemon.Kind == "routerd-eventrule" || !in.ExpiresAt.IsZero() && now.After(in.ExpiresAt) {
		return finish(s, nil)
	}
	in.Event.Time = in.Event.Time.UTC()
	key, ok := p.correlation(in)
	if !ok {
		if s.Warnings == math.MaxUint64 {
			return Result{}, ErrCapacity
		}
		s.Warnings++
		return finish(s, nil)
	}
	c := s.Keys[key]
	if c == nil {
		if len(s.Keys) == maxKeys {
			oldest := ""
			var at time.Time
			first := true
			for candidate, value := range s.Keys {
				if first || value.LastSeen.Before(at) || value.LastSeen.Equal(at) && candidate < oldest {
					oldest, at, first = candidate, value.LastSeen, false
				}
			}
			delete(s.Keys, oldest)
		}
		c = &correlation{}
		s.Keys[key] = c
	}
	c.LastSeen = in.Event.Time
	var outputs []Emission
	emit := func(count int) error {
		event, err := p.emit(&s, key, in, count, in.Event.Time)
		if err == nil {
			outputs = append(outputs, event)
		}
		return err
	}
	switch p.spec.Pattern.Operator {
	case "all_of":
		if p.exact(in.Event.Type) {
			if c.Seen == nil {
				c.Seen = map[string]bool{}
			}
			c.Seen[in.Event.Type] = true
			ready := true
			for _, topic := range p.topics {
				ready = ready && c.Seen[topic]
			}
			if ready {
				err = emit(0)
			}
		}
	case "any_of":
		if p.matches(in.Event.Type) {
			err = emit(0)
		}
	case "sequence":
		if in.Event.Type == p.topics[c.Sequence] {
			c.Sequence++
			if c.Sequence == len(p.topics) {
				c.Sequence = 0
				err = emit(0)
			}
		} else if p.spec.Pattern.Strict && p.exact(in.Event.Type) {
			c.Sequence = 0
		}
	case "window":
		if p.matches(in.Event.Type) {
			cutoff := in.Event.Time.Add(-p.delay)
			c.Window = append(c.Window, in.Event.Time)
			kept := c.Window[:0]
			for _, at := range c.Window {
				if !at.Before(cutoff) {
					kept = append(kept, at)
				}
			}
			c.Window = kept
			if len(kept) >= p.threshold {
				err = emit(len(kept))
				c.Window = nil
			}
		}
	case "count":
		if p.matches(in.Event.Type) {
			c.Count++
			if c.Count == p.threshold {
				err = emit(c.Count)
				c.Count = 0
			}
		}
	case "throttle":
		if p.matches(in.Event.Type) && (c.LastEmit.IsZero() || in.Event.Time.Sub(c.LastEmit) >= p.delay) {
			c.LastEmit = in.Event.Time
			err = emit(0)
		}
	case "absence":
		if bus.MatchTopic(p.spec.Pattern.Expected, in.Event.Type) {
			c.Pending = nil
		} else {
			trigger := p.spec.Pattern.Trigger
			if trigger == "" {
				trigger = p.spec.Pattern.Topic
			}
			if bus.MatchTopic(trigger, in.Event.Type) {
				c.Pending = &pending{Due: in.Event.Time.Add(p.delay), Input: in}
			}
		}
	case "debounce":
		if p.matches(in.Event.Type) {
			c.Pending = &pending{Due: in.Event.Time.Add(p.delay), Input: in}
		}
	}
	if err != nil {
		return Result{}, err
	}
	return finish(s, outputs)
}

// Tick claims due historical timers in a deterministic order, at most 64 per
// transaction. Timer goroutines and private process memory are not authority.
// The caller still needs the checkpoint version to commit this result.
func (p Program) Tick(previous json.RawMessage, now time.Time) (Result, error) {
	if now.IsZero() {
		return Result{}, errors.New("event rule timer time is required")
	}
	s, err := p.load(previous)
	if err != nil {
		return Result{}, err
	}
	var keys []string
	for key, c := range s.Keys {
		if c.Pending != nil && !now.Before(c.Pending.Due) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.Keys[keys[i]].Pending.Due, s.Keys[keys[j]].Pending.Due
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	var outputs []Emission
	for _, key := range keys[:min(len(keys), maxOutputs)] {
		timer := s.Keys[key].Pending
		s.Keys[key].Pending = nil
		if !timer.Input.ExpiresAt.IsZero() && now.After(timer.Input.ExpiresAt) {
			continue
		}
		out, err := p.emit(&s, key, timer.Input, 0, timer.Due)
		if err != nil {
			return Result{}, err
		}
		outputs = append(outputs, out)
	}
	return finish(s, outputs)
}

func (p Program) emit(s *state, key string, in Input, count int, at time.Time) (Emission, error) {
	if s.Fired == math.MaxUint64 {
		return Emission{}, ErrCapacity
	}
	s.Fired++
	body, _ := json.Marshal([]any{p.digest, key, in.ID, s.Fired})
	digest := sha256.Sum256(body)
	event := daemonapi.DaemonEvent{TypeMeta: daemonapi.TypeMeta{APIVersion: daemonapi.APIVersion, Kind: daemonapi.KindDaemonEvent}, Time: at.UTC(), Daemon: daemonapi.DaemonRef{Name: "routerd-eventrule", Kind: "routerd-eventrule", Instance: p.name}, Resource: &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "EventRule", Name: p.name}, Type: p.spec.Emit.Topic, Severity: daemonapi.SeverityInfo, Reason: "EventRuleFired", Message: "event rule fired", Attributes: map[string]string{}}
	for k, v := range p.spec.Emit.Attributes {
		event.Attributes[k] = render(v, in, key, count)
		if len(event.Attributes[k]) > 4096 {
			return Emission{}, ErrCapacity
		}
	}
	if count > 0 {
		event.Attributes["count"] = strconv.Itoa(count)
	}
	if key != "" {
		event.Attributes["correlation"] = key
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return Emission{}, err
	}
	if len(encoded) > 32<<10 {
		return Emission{}, ErrCapacity
	}
	return Emission{ID: hex.EncodeToString(digest[:]), Event: event}, nil
}

// Interpolation is one pass. A value supplied by an input cannot introduce a
// second expression whose expansion depends on Go map iteration order.
func render(template string, in Input, correlation string, count int) string {
	var out strings.Builder
	for {
		start := strings.Index(template, "${")
		if start < 0 {
			out.WriteString(template)
			break
		}
		out.WriteString(template[:start])
		template = template[start:]
		end := strings.IndexByte(template, '}')
		if end < 0 {
			out.WriteString(template)
			break
		}
		expression := template[2:end]
		value, known := "", true
		switch expression {
		case "event.type":
			value = in.Event.Type
		case "correlation":
			value = correlation
		case "count":
			value = strconv.Itoa(count)
		case "resource.name", "resource.kind", "resource.apiVersion":
			if in.Event.Resource == nil {
				known = false
			} else {
				switch expression {
				case "resource.name":
					value = in.Event.Resource.Name
				case "resource.kind":
					value = in.Event.Resource.Kind
				default:
					value = in.Event.Resource.APIVersion
				}
			}
		default:
			if strings.HasPrefix(expression, "attributes.") {
				value, known = in.Event.Attributes[strings.TrimPrefix(expression, "attributes.")]
			} else {
				known = false
			}
		}
		if known {
			out.WriteString(value)
		} else {
			out.WriteString(template[:end+1])
		}
		template = template[end+1:]
	}
	return out.String()
}
