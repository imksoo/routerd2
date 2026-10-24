// Package protocolevents relays durable historical protocol observations.
// It never transports current protocol authority or kernel effects.
package protocolevents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

const (
	MaxPending   = 64
	PageSize     = 16
	producerName = "health-probe-relay"
)

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

type Batch struct {
	Producer serviceconfig.Update `json:"producer"`
	Events   []events.Event       `json:"events"`
}

type Acknowledgement struct {
	IDs []string `json:"accepted_ids"`
}

type queue struct {
	SchemaVersion int            `json:"schema_version"`
	Pending       []events.Event `json:"pending"`
}

type Outbox struct {
	store   *events.Store
	name    string
	flushMu sync.Mutex
}

func NewOutbox(store *events.Store, name string) *Outbox { return &Outbox{store: store, name: name} }

// Pending returns an owned observation copy without acknowledging any entry.
func (o *Outbox) Pending(ctx context.Context) ([]events.Event, error) {
	_, q, err := o.read(ctx)
	return q.Pending, err
}

// ValidateHealth binds history to this worker's HealthCheck. The envelope is
// intentionally narrower than a generic journal append API. Its original
// timestamp may predate this boot/configuration; it is never a finite lease.
func ValidateHealth(name string, e events.Event) error {
	prefix := "health/" + name + "/"
	if !serviceName.MatchString(name) || !strings.HasPrefix(e.ID, prefix) || len(e.ID) != len(prefix)+32 {
		return errors.New("invalid health observation identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(e.ID, prefix)); err != nil {
		return errors.New("invalid health observation nonce")
	}
	if e.Source != old.DaemonKind || !e.ExpiresAt.IsZero() {
		return errors.New("invalid historical health source or expiry")
	}
	switch e.Topic {
	case "routerd.healthcheck." + name + "." + old.ResultPassed, "routerd.healthcheck." + name + "." + old.ResultFailed, "routerd.healthcheck." + name + "." + old.ResultTimeout:
	default:
		return errors.New("health observation topic is outside its resource")
	}
	d := e.Detail
	if d == nil || d.Daemon.Kind != old.DaemonKind || d.Daemon.Name != old.DaemonKind+"-"+name || d.Daemon.Instance != name || d.Resource == nil || d.Resource.APIVersion != api.NetAPIVersion || d.Resource.Kind != "HealthCheck" || d.Resource.Name != name || d.Reason != "HealthCheckProbe" || d.Severity != daemonapi.SeverityInfo {
		return errors.New("health observation detail is outside its resource")
	}
	return events.Validate(e)
}

func (o *Outbox) read(ctx context.Context) (events.ProducerState, queue, error) {
	if o.store == nil || !serviceName.MatchString(o.name) {
		return events.ProducerState{}, queue{}, errors.New("invalid health relay owner")
	}
	cp, err := o.store.ProducerCheckpoint(ctx, producerName)
	if err != nil {
		return cp, queue{}, err
	}
	if cp.Version == 0 && bytes.Equal(cp.State, []byte(`{}`)) {
		return cp, queue{SchemaVersion: 1}, nil
	}
	var q queue
	d := json.NewDecoder(bytes.NewReader(cp.State))
	d.DisallowUnknownFields()
	if err = d.Decode(&q); err != nil {
		return cp, q, err
	}
	if d.Decode(new(any)) != io.EOF || q.SchemaVersion != 1 || len(q.Pending) > MaxPending {
		return cp, q, errors.New("invalid health relay checkpoint")
	}
	seen := map[string]bool{}
	for _, e := range q.Pending {
		if err = ValidateHealth(o.name, e); err != nil {
			return cp, q, err
		}
		if seen[e.ID] {
			return cp, q, errors.New("duplicate health relay identity")
		}
		seen[e.ID] = true
	}
	return cp, q, nil
}

func (o *Outbox) save(ctx context.Context, cp events.ProducerState, q queue) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(body) > 64<<10 {
		return events.ErrCapacity
	}
	// The bounded pending queue is producer state, not permanent output jobs.
	// ACKed entries release local space; the target journal retains stable-ID
	// deduplication. Existing Store job tombstone semantics remain unchanged.
	return o.store.ProducerTransition(ctx, producerName, cp, body, nil)
}

func (o *Outbox) Enqueue(ctx context.Context, d daemonapi.DaemonEvent) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	e := events.FromDaemon("health/"+o.name+"/"+hex.EncodeToString(nonce[:]), d)
	if err := ValidateHealth(o.name, e); err != nil {
		return err
	}
	for range 8 {
		cp, q, err := o.read(ctx)
		if err != nil {
			return err
		}
		if len(q.Pending) >= MaxPending {
			return events.ErrCapacity
		}
		q.Pending = append(q.Pending, e)
		err = o.save(ctx, cp, q)
		if !errors.Is(err, events.ErrConflict) {
			return err
		}
	}
	return events.ErrConflict
}

// Flush acknowledges only the frozen prefix after the target confirms it.
// Concurrent enqueue is retained. A lost ACK or local commit failure retries
// identical IDs/content after restart, independently of current protocol facts.
func (o *Outbox) Flush(ctx context.Context, send func(context.Context, []events.Event) error) error {
	o.flushMu.Lock()
	defer o.flushMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, q, err := o.read(ctx)
	if err != nil {
		return err
	}
	if len(q.Pending) == 0 {
		return nil
	}
	page := q.Pending[:min(PageSize, len(q.Pending))]
	if err = send(ctx, page); err != nil {
		return err
	}
	for range 8 {
		cp, current, err := o.read(ctx)
		if err != nil {
			return err
		}
		if len(current.Pending) < len(page) {
			return events.ErrConflict
		}
		for i, e := range page {
			if current.Pending[i].ID != e.ID {
				return events.ErrConflict
			}
		}
		current.Pending = current.Pending[len(page):]
		err = o.save(ctx, cp, current)
		if !errors.Is(err, events.ErrConflict) {
			return err
		}
	}
	return events.ErrConflict
}
