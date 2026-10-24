package protocolevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

const retirementProducer = "control-health-relays"
const MaxRegisteredRelays = 1024
const recoveryBatch = 8

type relayRegistry struct {
	SchemaVersion int      `json:"schema_version"`
	Names         []string `json:"names"`
}

type RetirementStatus struct {
	Registered int `json:"registered_relays"`
	Retiring   int `json:"retiring_relays"`
}

// Retirement is serialized by the caller's configuration authority lock.
// Only registered, no-longer-declared relays may be collected. A supervisor
// guard and the local store's exclusive lock independently fence ownership.
type Retirement struct {
	journal  *events.Store
	root     string
	names    []string
	declared map[string]bool
	status   RetirementStatus
	errors   map[string]error
	next     int
}

func NewRetirement(journal *events.Store, root string) *Retirement {
	return &Retirement{journal: journal, root: root, errors: map[string]error{}}
}

func (r *Retirement) Status() RetirementStatus { return r.status }

func (r *Retirement) Declare(names []string) {
	r.declared = map[string]bool{}
	for _, name := range names {
		r.declared[name] = true
	}
	r.refreshStatus()
}

func (r *Retirement) refreshStatus() {
	r.status = RetirementStatus{Registered: len(r.names)}
	for _, name := range r.names {
		if !r.declared[name] {
			r.status.Retiring++
		}
	}
}

func (r *Retirement) load(ctx context.Context) (events.ProducerState, bool, error) {
	cp, found, err := r.journal.LookupProducer(ctx, retirementProducer)
	if err != nil {
		return cp, found, err
	}
	var registry relayRegistry
	if !found || cp.Version == 0 && bytes.Equal(cp.State, []byte(`{}`)) {
		registry.SchemaVersion = 1
	} else {
		d := json.NewDecoder(bytes.NewReader(cp.State))
		d.DisallowUnknownFields()
		if err = d.Decode(&registry); err != nil {
			return cp, found, err
		}
		if d.Decode(new(any)) != io.EOF || registry.SchemaVersion != 1 || len(registry.Names) > MaxRegisteredRelays {
			return cp, found, errors.New("invalid health relay registry")
		}
		for i, name := range registry.Names {
			if !serviceName.MatchString(name) || i > 0 && registry.Names[i-1] >= name {
				return cp, found, errors.New("invalid health relay registry names")
			}
		}
	}
	r.names = registry.Names
	for name := range r.errors {
		if !slices.Contains(r.names, name) {
			delete(r.errors, name)
		}
	}
	r.refreshStatus()
	return cp, found, nil
}

func (r *Retirement) save(ctx context.Context, cp events.ProducerState, names []string) error {
	body, err := json.Marshal(relayRegistry{SchemaVersion: 1, Names: names})
	if err != nil {
		return err
	}
	if err = r.journal.ProducerTransition(ctx, retirementProducer, cp, body, nil); err != nil {
		return err
	}
	r.names = names
	r.refreshStatus()
	return nil
}

func initializeRelay(ctx context.Context, root, name string) error {
	opts := events.Options{Directory: serviceconfig.HealthEventDirectory(root, name)}
	store, err := events.Open(opts)
	if errors.Is(err, events.ErrOwned) {
		// An already-running worker from an earlier control incarnation may
		// own this queue. Confirm that established state exists before tracking
		// it; do not equate an initializing/missing database with a valid queue.
		store, err = events.OpenExisting(opts)
		if errors.Is(err, events.ErrOwned) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	_, readErr := NewOutbox(store, name).Pending(ctx)
	return errors.Join(readErr, store.Close())
}

// Register establishes queues and persists their names before any worker can
// start. A crash before the registry commit cannot leave an accepted probe in
// an undiscoverable queue. Nothing scans or adopts unrelated filesystem state.
func (r *Retirement) Register(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if !serviceName.MatchString(name) {
			return errors.New("invalid health relay name")
		}
	}
	cp, found, err := r.load(ctx)
	if err != nil {
		return err
	}
	union := append([]string(nil), r.names...)
	for _, name := range names {
		if !slices.Contains(union, name) {
			union = append(union, name)
		}
	}
	if len(union) > MaxRegisteredRelays {
		return events.ErrCapacity
	}
	if len(union) == len(r.names) {
		return nil
	}
	for _, name := range union {
		if !slices.Contains(r.names, name) {
			if err = initializeRelay(ctx, r.root, name); err != nil {
				return fmt.Errorf("initialize health relay %s: %w", name, err)
			}
		}
	}
	sort.Strings(union)
	if !found {
		cp, err = r.journal.ProducerCheckpoint(ctx, retirementProducer)
		if err != nil {
			return err
		}
	}
	return r.save(ctx, cp, union)
}

func (r *Retirement) remove(ctx context.Context, name string) error {
	cp, _, err := r.load(ctx)
	if err != nil {
		return err
	}
	i := slices.Index(r.names, name)
	if i < 0 {
		return nil
	}
	next := append([]string(nil), r.names[:i]...)
	next = append(next, r.names[i+1:]...)
	return r.save(ctx, cp, next)
}

func (r *Retirement) recoverOne(ctx context.Context, name string) error {
	store, err := events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(r.root, name)})
	if errors.Is(err, events.ErrOwned) {
		return nil
	} // still owned; retain registration
	if err != nil {
		return err
	}
	defer store.Close()
	box := NewOutbox(store, name)
	err = box.Flush(ctx, func(ctx context.Context, page []events.Event) error {
		for _, event := range page {
			if _, err := r.journal.Append(ctx, event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	pending, err := box.Pending(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return nil
	}
	// Keep the queue exclusively owned until the registry ACK is durable.
	return r.remove(ctx, name)
}

func (r *Retirement) Recover(ctx context.Context, declared []string, withStopped func(context.Context, []string, func(map[string]bool) error) error) error {
	if _, _, err := r.load(ctx); err != nil {
		return err
	}
	r.Declare(declared)
	retiring := []string{}
	for _, name := range r.names {
		if !r.declared[name] {
			retiring = append(retiring, name)
		}
	}
	if len(retiring) == 0 {
		return nil
	}
	selected := []string{}
	for i := 0; i < min(recoveryBatch, len(retiring)); i++ {
		selected = append(selected, retiring[(r.next+i)%len(retiring)])
	}
	r.next = (r.next + len(selected)) % len(retiring)
	if err := withStopped(ctx, selected, func(allowed map[string]bool) error {
		for _, name := range selected {
			if !allowed[name] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.recoverOne(ctx, name); err != nil {
				r.errors[name] = err
			} else {
				delete(r.errors, name)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	r.refreshStatus()
	names := []string{}
	for name := range r.errors {
		names = append(names, name)
	}
	sort.Strings(names)
	var result error
	for _, name := range names {
		result = errors.Join(result, fmt.Errorf("recover retired health relay %s: %w", name, r.errors[name]))
	}
	return result
}
