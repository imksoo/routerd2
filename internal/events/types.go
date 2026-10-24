// Package events stores durable observations and consumer work independently
// of transient protocol facts and the lossy logging transport.
package events

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrConflict = errors.New("event identity or consumer cursor conflict")
	ErrCapacity = errors.New("durable event storage capacity exhausted")
	ErrClosed   = errors.New("durable event store is closed")
)

const maxBody = 64 << 10

type Options struct {
	Directory                        string
	MaxEvents, MaxJobs, MaxConsumers int64
	MaxEventBytes, MaxJobBytes       int64
	MaxConsumerStateBytes            int64
}

// Event is historical evidence, never a command or permission to mutate the
// kernel. In particular, zero ExpiresAt is a non-expiring event, not a lease.
type Event struct {
	ID         string            `json:"id"`
	Topic      string            `json:"topic"`
	Source     string            `json:"source"`
	ObservedAt time.Time         `json:"observed_at"`
	ExpiresAt  time.Time         `json:"expires_at,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

func (e Event) IsExpired(now time.Time) bool { return !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) }

type Record struct {
	Cursor uint64 `json:"cursor"`
	Event  Event  `json:"event"`
}

type Job struct {
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload"`
}

type Delivery struct {
	Job
	EventCursor uint64 `json:"event_cursor"`
}

// Checkpoint is one consistent durable consumer state. Version also fences
// timer-driven changes which do not advance the source event cursor.
type Checkpoint struct {
	Cursor  uint64          `json:"cursor"`
	Version uint64          `json:"version"`
	State   json.RawMessage `json:"state"`
}

func token(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func encodeEvent(e Event) ([]byte, error) {
	if !token(e.ID, 256) || !token(e.Topic, 128) || !token(e.Source, 128) || e.ObservedAt.IsZero() || len(e.Attributes) > 64 || !e.ExpiresAt.IsZero() && e.ExpiresAt.Before(e.ObservedAt) {
		return nil, errors.New("invalid durable event envelope")
	}
	for key, value := range e.Attributes {
		if !token(key, 128) || len(value) > 4096 || !utf8.ValidString(value) {
			return nil, errors.New("event attribute exceeds its bound")
		}
	}
	e.ObservedAt = e.ObservedAt.UTC()
	if !e.ExpiresAt.IsZero() {
		e.ExpiresAt = e.ExpiresAt.UTC()
	}
	body, err := json.Marshal(e)
	if err == nil && len(body) > maxBody {
		err = errors.New("event exceeds 64 KiB")
	}
	return body, err
}
