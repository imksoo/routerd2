// Package derivedcontroller persists sampled current-state evaluations and
// historical outputs. A durable asserted value is never current authority.
package derivedcontroller

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

	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

const registryName = "control-derived-lifecycles"
const evaluatorLease = uint64(5 * time.Second)

type registration struct {
	Identity string `json:"identity"`
	Lifetime uint64 `json:"lifetime"`
	Producer string `json:"producer"`
}
type registry struct {
	Revision uint64                  `json:"revision"`
	Counter  uint64                  `json:"counter"`
	Entries  map[string]registration `json:"entries"`
}
type binding struct {
	registration
	program derived.Program
	until   uint64
}
type cached struct {
	body     json.RawMessage
	result   derived.Result
	identity string
	lease    uint64
	version  uint64
	pending  bool
}

type Observation struct {
	ID              string         `json:"id"`
	Lifetime        uint64         `json:"lifetime"`
	Version         uint64         `json:"version"`
	State           derived.Status `json:"state"`
	ObservedNanos   uint64         `json:"observed_nanos"`
	ValidUntilNanos uint64         `json:"valid_until_nanos"`
	Pending         bool           `json:"pending"`
}
type Health struct {
	Revision uint64 `json:"revision"`
	Active   int    `json:"active"`
	Pending  bool   `json:"pending"`
}
type Controller struct {
	store    *events.Store
	revision uint64
	bindings map[string]binding
	cache    map[string]cached
	order    []string
	start    int
}

func New(store *events.Store) *Controller {
	return &Controller{store: store, cache: map[string]cached{}}
}
func producerName(id, digest string, lifetime uint64) string {
	b, _ := json.Marshal([]any{id, digest, lifetime})
	sum := sha256.Sum256(b)
	return "derived/" + hex.EncodeToString(sum[:])
}

func (c *Controller) Sync(ctx context.Context, revision uint64, declarations []resourceprogram.DerivedEvent) (err error) {
	defer func() {
		if err != nil {
			c.bindings = nil
			c.order = nil
		}
	}()
	if c.store == nil || revision == 0 || len(declarations) > 128 {
		return errors.New("invalid derived controller source")
	}
	wanted := map[string]binding{}
	for _, declaration := range declarations {
		if _, exists := wanted[declaration.ID]; exists {
			return errors.New("duplicate derived resource")
		}
		program, err := derived.Compile(declaration.ID, declaration.Spec)
		if err != nil {
			return fmt.Errorf("derived event %s: %w", declaration.ID, err)
		}
		body, err := json.Marshal(declaration)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		wanted[declaration.ID] = binding{registration: registration{Identity: hex.EncodeToString(sum[:])}, program: program, until: declaration.ActiveUntilNanos}
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
		return errors.New("trailing derived registry")
	}
	if len(old.Entries) > 128 {
		return events.ErrCapacity
	}
	if old.Revision == 0 && (old.Counter != 0 || len(old.Entries) != 0) {
		return errors.New("invalid derived registry revision")
	}
	for id, entry := range old.Entries {
		if entry.Lifetime == 0 || entry.Lifetime > old.Counter || entry.Producer != producerName(id, entry.Identity, entry.Lifetime) {
			return errors.New("invalid derived lifecycle registry")
		}
	}
	next := registry{Revision: revision, Counter: old.Counter, Entries: map[string]registration{}}
	var order []string
	for id := range wanted {
		order = append(order, id)
	}
	sort.Strings(order)
	gap := old.Revision != 0 && old.Revision != revision && (old.Revision == math.MaxUint64 || revision != old.Revision+1)
	nextCache := map[string]cached{}
	for _, id := range order {
		b := wanted[id]
		entry, exists := old.Entries[id]
		if !exists || gap || entry.Identity != b.Identity {
			if next.Counter == math.MaxUint64 {
				return events.ErrCapacity
			}
			next.Counter++
			entry = registration{Identity: b.Identity, Lifetime: next.Counter}
			entry.Producer = producerName(id, entry.Identity, entry.Lifetime)
			if _, err = c.store.ProducerCheckpoint(ctx, entry.Producer); err != nil {
				return err
			}
		}
		if prior, exists := c.bindings[id]; exists && prior.Producer == entry.Producer {
			nextCache[id] = c.cache[id]
		}
		b.registration = entry
		wanted[id] = b
		next.Entries[id] = entry
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
	c.revision = revision
	c.bindings = wanted
	c.order = order
	c.cache = nextCache
	return nil
}

func (c *Controller) Step(ctx context.Context, snapshots map[string]derived.Snapshot) error {
	if len(c.order) == 0 {
		return nil
	}
	for range min(len(c.order), 16) {
		index := c.start % len(c.order)
		c.start = (index + 1) % len(c.order)
		id := c.order[index]
		snapshot, exists := snapshots[id]
		if !exists {
			return fmt.Errorf("derived event %s requires a current snapshot", id)
		}
		if err := c.stepOne(ctx, id, c.bindings[id], snapshot); err != nil {
			return fmt.Errorf("derived event %s: %w", id, err)
		}
	}
	return nil
}

func (c *Controller) stepOne(ctx context.Context, id string, b binding, snapshot derived.Snapshot) error {
	_, at := snapshot.Timebase()
	if b.until != 0 && at >= b.until {
		delete(c.cache, id)
		return nil
	}
	cp, err := c.store.ProducerCheckpoint(ctx, b.Producer)
	if err != nil {
		return err
	}
	result, err := b.program.Advance(cp.State, snapshot, false)
	if err != nil {
		return err
	}
	changed, err := b.program.CheckpointChanged(cp.State, result.State)
	if err != nil {
		return err
	}
	if changed {
		var jobs []events.Job
		for _, emission := range result.Emissions {
			body, err := json.Marshal(emission)
			if err != nil {
				return err
			}
			jobs = append(jobs, events.Job{Key: emission.ID, Payload: body})
		}
		if err = c.store.ProducerTransition(ctx, b.Producer, cp, result.State, jobs); err != nil {
			return err
		}
		cp.Version++
	}
	_, now := snapshot.Timebase()
	lease := uint64(math.MaxUint64)
	if now < math.MaxUint64-evaluatorLease {
		lease = now + evaluatorLease
	}
	if b.until != 0 {
		lease = min(lease, b.until)
	}
	c.cache[id] = cached{body: result.State, result: result, identity: b.program.InputIdentity(snapshot), lease: lease, version: cp.Version}
	work, err := c.store.ProducerPending(ctx, b.Producer, 8)
	if err != nil {
		return err
	}
	entry := c.cache[id]
	entry.pending = len(work) > 0
	c.cache[id] = entry
	for _, job := range work {
		var emission derived.Emission
		d := json.NewDecoder(bytes.NewReader(job.Payload))
		d.DisallowUnknownFields()
		if err = d.Decode(&emission); err != nil {
			return err
		}
		if d.Decode(new(any)) != io.EOF || emission.ID != job.Key {
			return errors.New("invalid derived output job")
		}
		key, _ := json.Marshal([]string{b.Producer, job.Key})
		sum := sha256.Sum256(key)
		if _, err = c.store.Append(ctx, events.FromDaemon(hex.EncodeToString(sum[:]), emission.Event)); err != nil {
			return err
		}
		if err = c.store.ProducerComplete(ctx, b.Producer, job.Key); err != nil {
			return err
		}
	}
	remaining, err := c.store.ProducerPending(ctx, b.Producer, 1)
	if err != nil {
		return err
	}
	entry = c.cache[id]
	entry.pending = len(remaining) > 0
	c.cache[id] = entry
	return nil
}

// View is read-only. Current input, producer continuity and evaluator liveness
// must all match; a new transition is not exposed until durably committed.
func (c *Controller) View(id string, snapshot derived.Snapshot) (Observation, bool) {
	b, exists := c.bindings[id]
	if !exists {
		return Observation{}, false
	}
	entry, hasState := c.cache[id]
	view := Observation{ID: id, Lifetime: b.Lifetime, Version: entry.version, State: derived.Status{Phase: "Indeterminate", Inputs: derived.Indeterminate, PendingTransition: "None"}, Pending: entry.pending}
	_, now := snapshot.Timebase()
	if !hasState || entry.identity != b.program.InputIdentity(snapshot) || now >= entry.lease {
		return view, true
	}
	preview, err := b.program.Advance(entry.body, snapshot, false)
	if err != nil {
		return view, true
	}
	changed, err := b.program.CheckpointChanged(entry.body, preview.State)
	if err != nil {
		return view, true
	}
	if !changed && len(preview.Emissions) == 0 {
		view.State = preview.Status
		view.ObservedNanos = preview.ObservedNanos
		view.ValidUntilNanos = min(preview.ValidUntilNanos, entry.lease)
	} else if now < entry.result.ValidUntilNanos {
		view.State = entry.result.Status
		view.ObservedNanos = entry.result.ObservedNanos
		view.ValidUntilNanos = min(entry.result.ValidUntilNanos, entry.lease)
	}
	if view.State.Asserted != nil {
		value := *view.State.Asserted
		view.State.Asserted = &value
	}
	return view, true
}

func (c *Controller) Health() Health {
	h := Health{Revision: c.revision, Active: len(c.bindings)}
	for _, entry := range c.cache {
		h.Pending = h.Pending || entry.pending
	}
	return h
}
