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

	"github.com/imksoo/routerd/pkg/daemonapi"
)

var (
	ErrConflict = errors.New("event identity or consumer cursor conflict")
	ErrCapacity = errors.New("durable event storage capacity exhausted")
	ErrClosed   = errors.New("durable event store is closed")
	ErrOwned    = errors.New("event store already owned")
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
	Detail     *Detail           `json:"detail,omitempty"`
}

// Detail preserves bounded historical daemon/resource metadata for rules.
// It does not carry protocol freshness or permission to execute an action.
type Detail struct {
	Daemon   daemonapi.DaemonRef    `json:"daemon"`
	Resource *daemonapi.ResourceRef `json:"resource,omitempty"`
	Severity string                 `json:"severity,omitempty"`
	Reason   string                 `json:"reason,omitempty"`
	Message  string                 `json:"message,omitempty"`
}

func FromDaemon(id string, d daemonapi.DaemonEvent) Event {
	return Event{ID: id, Topic: d.Type, Source: d.Daemon.Kind, ObservedAt: d.Time, Attributes: d.Attributes, Detail: &Detail{Daemon: d.Daemon, Resource: d.Resource, Severity: d.Severity, Reason: d.Reason, Message: d.Message}}
}

func (e Event) DaemonEvent() daemonapi.DaemonEvent {
	d := daemonapi.DaemonEvent{TypeMeta: daemonapi.TypeMeta{APIVersion: daemonapi.APIVersion, Kind: daemonapi.KindDaemonEvent}, Time: e.ObservedAt, Type: e.Topic, Attributes: e.Attributes, Daemon: daemonapi.DaemonRef{Name: e.Source, Kind: e.Source}}
	if e.Detail != nil {
		d.Daemon = e.Detail.Daemon
		d.Resource = e.Detail.Resource
		d.Severity = e.Detail.Severity
		d.Reason = e.Detail.Reason
		d.Message = e.Detail.Message
	}
	return d
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

// ProducerState is a versioned observation producer's durable state. It has
// no historical input cursor and does not authenticate a live producer.
type ProducerState struct {
	Version uint64          `json:"version"`
	State   json.RawMessage `json:"state"`
}

type ProducerDelivery struct {
	Job
	StateVersion uint64 `json:"state_version"`
}

func token(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// Validate checks the historical envelope without appending it or granting
// authority. Relays use the same byte/field limits before durable acceptance.
func Validate(e Event) error {
	_, err := encodeEvent(e)
	return err
}

func encodeEvent(e Event) ([]byte, error) {
	if !token(e.ID, 256) || !token(e.Topic, 128) || !token(e.Source, 128) || e.ObservedAt.IsZero() || len(e.Attributes) > 64 || !e.ExpiresAt.IsZero() && e.ExpiresAt.Before(e.ObservedAt) {
		return nil, errors.New("invalid durable event envelope")
	}
	if d := e.Detail; d != nil {
		if d.Daemon.Kind != e.Source || !token(d.Daemon.Name, 128) || len(d.Daemon.Instance) > 128 || !utf8.ValidString(d.Daemon.Instance) {
			return nil, errors.New("invalid historical daemon identity")
		}
		for _, value := range []string{d.Severity, d.Reason, d.Message} {
			if len(value) > 4096 || !utf8.ValidString(value) {
				return nil, errors.New("historical detail exceeds its bound")
			}
		}
		if d.Resource != nil {
			for _, value := range []string{d.Resource.APIVersion, d.Resource.Kind, d.Resource.Name} {
				if len(value) > 128 || !utf8.ValidString(value) {
					return nil, errors.New("invalid historical resource reference")
				}
			}
		}
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
