// Package eventcontroller connects pure historical rules to durable cursors,
// state and output publication. Its caller serializes source/lifecycle changes
// with processing; historical output is never protocol or kernel authority.
package eventcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/eventrule"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

const registryName = "control-eventrule-lifecycles"

type registration struct {
	Identity string `json:"identity"`
	Lifetime uint64 `json:"lifetime"`
	Consumer string `json:"consumer"`
}
type registry struct {
	Revision uint64                  `json:"revision"`
	Counter  uint64                  `json:"counter"`
	Entries  map[string]registration `json:"entries"`
}
type binding struct {
	id string
	registration
	program eventrule.Program
}
type RuleStatus struct {
	ID       string `json:"id"`
	Lifetime uint64 `json:"lifetime"`
	Cursor   uint64 `json:"cursor"`
	Version  uint64 `json:"version"`
	Pending  bool   `json:"pending"`
}
type Status struct {
	Revision uint64       `json:"revision"`
	Rules    []RuleStatus `json:"rules"`
}
type Controller struct {
	store    *events.Store
	bindings []binding
	status   Status
	start    int
}

func New(store *events.Store) *Controller { return &Controller{store: store} }

func consumerName(id, identity string, lifetime uint64) string {
	body, _ := json.Marshal([]any{id, identity, lifetime})
	sum := sha256.Sum256(body)
	return "eventrule/" + hex.EncodeToString(sum[:])
}

// Sync makes each activation durable. A new consumer is registered at the
// journal tip before the registry commit, so a crash in that interval reuses
// its cursor rather than silently skipping subsequently appended inputs.
// Unchanged definitions retain state across unrelated source revisions.
// An unobserved revision gap conservatively starts fresh lifetimes.
func (c *Controller) Sync(ctx context.Context, revision uint64, rules []resourceprogram.EventRule) (err error) {
	defer func() {
		if err != nil {
			c.bindings = nil
		}
	}()
	if c.store == nil || revision == 0 || len(rules) > 128 {
		return errors.New("invalid event rule controller source")
	}
	programs := map[string]eventrule.Program{}
	identities := map[string]string{}
	for _, rule := range rules {
		if _, duplicate := programs[rule.ID]; duplicate {
			return errors.New("duplicate event rule resource")
		}
		p, compileErr := eventrule.Compile(rule.ID, rule.Spec)
		if compileErr != nil {
			return fmt.Errorf("event rule %s: %w", rule.ID, compileErr)
		}
		body, encodeErr := json.Marshal(rule)
		if encodeErr != nil {
			return encodeErr
		}
		sum := sha256.Sum256(body)
		programs[rule.ID] = p
		identities[rule.ID] = hex.EncodeToString(sum[:])
	}
	cp, err := c.store.ProducerCheckpoint(ctx, registryName)
	if err != nil {
		return err
	}
	var old registry
	d := json.NewDecoder(bytes.NewReader(cp.State))
	d.DisallowUnknownFields()
	if err = d.Decode(&old); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing event rule registry")
	}
	if len(old.Entries) > 128 {
		return events.ErrCapacity
	}
	if old.Revision == 0 && (old.Counter != 0 || len(old.Entries) != 0) {
		return errors.New("invalid event rule registry revision")
	}
	for id, entry := range old.Entries {
		if entry.Lifetime == 0 || entry.Lifetime > old.Counter || entry.Consumer != consumerName(id, entry.Identity, entry.Lifetime) {
			return errors.New("invalid event rule lifecycle registry")
		}
	}
	next := registry{Revision: revision, Counter: old.Counter, Entries: map[string]registration{}}
	var ids []string
	for id := range programs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	gap := old.Revision != 0 && old.Revision != revision && (old.Revision == math.MaxUint64 || revision != old.Revision+1)
	var bindings []binding
	for _, id := range ids {
		entry, exists := old.Entries[id]
		if !exists || gap || entry.Identity != identities[id] {
			if next.Counter == math.MaxUint64 {
				return events.ErrCapacity
			}
			next.Counter++
			entry = registration{Identity: identities[id], Lifetime: next.Counter}
			entry.Consumer = consumerName(id, entry.Identity, entry.Lifetime)
			if _, err = c.store.Checkpoint(ctx, entry.Consumer); err != nil {
				return err
			}
		}
		next.Entries[id] = entry
		bindings = append(bindings, binding{id: id, registration: entry, program: programs[id]})
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, cp.State) {
		if err = c.store.ProducerTransition(ctx, registryName, cp, body, nil); err != nil {
			return err
		}
	}
	oldStatuses := map[string]RuleStatus{}
	for _, status := range c.status.Rules {
		oldStatuses[status.ID] = status
	}
	c.bindings = bindings
	c.status.Revision = revision
	c.status.Rules = nil
	for _, b := range bindings {
		status := oldStatuses[b.id]
		if status.Lifetime != b.Lifetime {
			status = RuleStatus{ID: b.id, Lifetime: b.Lifetime}
		}
		c.status.Rules = append(c.status.Rules, status)
	}
	return nil
}

// Step processes bounded pages and rotates its starting rule. Call Sync with
// the current active set first, under the same source-authority lock. Failed
// publication/ACK is retried using the original immutable outbox payload.
func (c *Controller) Step(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("event controller time is required")
	}
	if len(c.bindings) == 0 {
		return nil
	}
	for range min(len(c.bindings), 16) {
		index := c.start % len(c.bindings)
		c.start = (index + 1) % len(c.bindings)
		if err := c.stepRule(ctx, c.bindings[index], index, now); err != nil {
			return fmt.Errorf("event rule %s: %w", c.bindings[index].id, err)
		}
	}
	return nil
}

func jobs(result eventrule.Result) ([]events.Job, error) {
	out := make([]events.Job, 0, len(result.Emissions))
	for _, emission := range result.Emissions {
		body, err := json.Marshal(emission)
		if err != nil {
			return nil, err
		}
		out = append(out, events.Job{Key: emission.ID, Payload: body})
	}
	return out, nil
}

func (c *Controller) stepRule(ctx context.Context, b binding, index int, now time.Time) error {
	cp, err := c.store.Checkpoint(ctx, b.Consumer)
	if err != nil {
		return err
	}
	rows, err := c.store.List(ctx, cp.Cursor, 4)
	if err != nil {
		return err
	}
	for _, row := range rows {
		cutoff := now
		if before := row.Event.ObservedAt.Add(-time.Nanosecond); before.Before(cutoff) {
			cutoff = before
		}
		var caughtUp bool
		cp, caughtUp, err = c.tick(ctx, b, cp, cutoff)
		if err != nil {
			return err
		}
		if !caughtUp {
			break
		}
		result, err := b.program.Advance(cp.State, eventrule.Input{ID: row.Event.ID, Event: row.Event.DaemonEvent(), ExpiresAt: row.Event.ExpiresAt}, now)
		if err != nil {
			return err
		}
		outputs, err := jobs(result)
		if err != nil {
			return err
		}
		if err = c.store.Transition(ctx, b.Consumer, cp, row.Cursor, result.State, outputs); err != nil {
			return err
		}
		cp, err = c.store.Checkpoint(ctx, b.Consumer)
		if err != nil {
			return err
		}
	}
	// A queued observation wins at an equal deadline; earlier timers run
	// before later observations even when processing was paused or backlogged.
	remaining, err := c.store.List(ctx, cp.Cursor, 1)
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		cp, _, err = c.tick(ctx, b, cp, now)
		if err != nil {
			return err
		}
	}
	work, err := c.store.Pending(ctx, b.Consumer, 8)
	if err != nil {
		return err
	}
	c.status.Rules[index] = RuleStatus{ID: b.id, Lifetime: b.Lifetime, Cursor: cp.Cursor, Version: cp.Version, Pending: len(work) > 0}
	for _, job := range work {
		var emission eventrule.Emission
		d := json.NewDecoder(bytes.NewReader(job.Payload))
		d.DisallowUnknownFields()
		if err = d.Decode(&emission); err != nil {
			return err
		}
		if d.Decode(new(any)) != io.EOF || emission.ID != job.Key {
			return errors.New("invalid event rule output job")
		}
		identity, _ := json.Marshal([]string{b.Consumer, job.Key})
		sum := sha256.Sum256(identity)
		if _, err = c.store.Append(ctx, events.FromDaemon(hex.EncodeToString(sum[:]), emission.Event)); err != nil {
			return err
		}
		if err = c.store.Complete(ctx, b.Consumer, job.Key); err != nil {
			return err
		}
	}
	remainingWork, err := c.store.Pending(ctx, b.Consumer, 1)
	if err != nil {
		return err
	}
	c.status.Rules[index].Pending = len(remainingWork) > 0
	return nil
}

func (c *Controller) tick(ctx context.Context, b binding, cp events.Checkpoint, at time.Time) (events.Checkpoint, bool, error) {
	result, err := b.program.Tick(cp.State, at)
	if err != nil {
		return cp, false, err
	}
	if bytes.Equal(cp.State, result.State) {
		return cp, true, nil
	}
	outputs, err := jobs(result)
	if err != nil {
		return cp, false, err
	}
	if err = c.store.Transition(ctx, b.Consumer, cp, cp.Cursor, result.State, outputs); err != nil {
		return cp, false, err
	}
	cp, err = c.store.Checkpoint(ctx, b.Consumer)
	if err != nil {
		return cp, false, err
	}
	// A bounded timer batch must finish before advancing past this input.
	probe, err := b.program.Tick(cp.State, at)
	if err != nil {
		return cp, false, err
	}
	return cp, bytes.Equal(cp.State, probe.State), nil
}

func (c *Controller) Status() Status {
	out := c.status
	out.Rules = append([]RuleStatus(nil), out.Rules...)
	return out
}
