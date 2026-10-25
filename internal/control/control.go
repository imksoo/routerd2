// Package control is the single writer of committed configuration. All network
// changes pass through its revision check; the network worker owns kernel objects.
package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/eventcontroller"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

const SocketName = "control.sock"
const NetworkSocketName = "network.sock"

type ApplyRequest struct {
	Revision uint64          `json:"revision"`
	Desired  network.Desired `json:"desired"`
}

// ApplyResult acknowledges a durable kernel projection without returning its
// full desired state and observations. Those can be much larger than the apply
// request (for example WireGuard peers appear in both forms). A successful
// mutation must not become an apparent failure solely because its response
// exceeds the internal API's message bound.
type ApplyResult struct {
	Revision          uint64 `json:"revision"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	Status            string `json:"status"`
}

type CommitRequest struct {
	ExpectedRevision      uint64          `json:"expected_revision"`
	RequestID             string          `json:"request_id"`
	Config                config.Document `json:"config"`
	ConfirmTimeoutSeconds int             `json:"confirm_timeout_seconds,omitempty"`
}

type ConfirmRequest struct {
	Revision  uint64 `json:"revision"`
	RequestID string `json:"request_id"`
}

type State struct {
	config.Committed
	Healthy            bool                `json:"healthy"`
	LastError          string              `json:"last_error,omitempty"`
	LastErrorTruncated bool                `json:"last_error_truncated,omitempty"`
	ProducerEpoch      string              `json:"producer_epoch"`
	Sequence           uint64              `json:"sequence"`
	Services           *runtimeplan.Status `json:"services,omitempty"`
	// Metadata and large successful mutation acknowledgments deliberately omit
	// document bodies. Clients must inspect these flags before using Config or
	// Confirmation.Previous; zero values are never partial configurations.
	ConfigOmitted               bool   `json:"config_omitted,omitempty"`
	ConfirmationPreviousOmitted bool   `json:"confirmation_previous_omitted,omitempty"`
	ServicesOmitted             bool   `json:"services_omitted,omitempty"`
	ConfigDigest                string `json:"config_digest,omitempty"`
	ConfigBytes                 int    `json:"config_bytes,omitempty"`
	EventJournalError           string `json:"event_journal_error,omitempty"`
	EventStoreError             string `json:"event_store_error,omitempty"`
	EventRuleError              string `json:"event_rule_error,omitempty"`
	DerivedError                string `json:"derived_error,omitempty"`
	ProtocolEventError          string `json:"protocol_event_error,omitempty"`
}

type Event struct {
	ProducerEpoch string `json:"producer_epoch"`
	Sequence      uint64 `json:"sequence"`
	Revision      uint64 `json:"config_revision"`
	RequestID     string `json:"request_id,omitempty"`
	Type          string `json:"type"`
}

type Changes struct {
	ProducerEpoch string  `json:"producer_epoch"`
	Sequence      uint64  `json:"sequence"`
	Reset         bool    `json:"reset"`
	Events        []Event `json:"events"`
}

type Network interface {
	Apply(context.Context, uint64, network.Desired) error
	Snapshot(context.Context) (network.Snapshot, error)
}

type NetworkClient struct{ client *localapi.Client }

func NewNetworkClient(dir string) *NetworkClient {
	return &NetworkClient{localapi.NewClient(filepath.Join(dir, NetworkSocketName))}
}
func (n *NetworkClient) Apply(ctx context.Context, rev uint64, d network.Desired) error {
	return n.client.Do(ctx, http.MethodPost, "/v1/apply", ApplyRequest{rev, d}, nil)
}
func (n *NetworkClient) Snapshot(ctx context.Context) (s network.Snapshot, err error) {
	v, err := n.Observe(ctx)
	if err != nil {
		return s, err
	}
	if v.Truncated || !v.Complete {
		return s, errors.New("network observation is incomplete; use bounded observation detail queries")
	}
	return v.Snapshot, nil
}

type Engine struct {
	mu             sync.Mutex
	store          *config.Store
	network        Network
	state          State
	events         []Event
	runtime        *runtimeplan.Runtime
	journal        *events.Store
	eventStore     EventStoreHealth
	eventFlushMu   sync.Mutex
	rules          *eventcontroller.Controller
	derived        *derivedcontroller.Controller
	protocolErrors map[string]string
	protocolRelays *protocolevents.Retirement
}

func New(store *config.Store, current config.Committed, n Network) *Engine {
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		panic(err)
	}
	return &Engine{store: store, network: n, state: State{Committed: current, ProducerEpoch: hex.EncodeToString(epoch[:])}}
}

func (e *Engine) State() State {
	e.mu.Lock()
	s := e.state
	e.mu.Unlock()
	if s.EventJournalError != "" || s.EventStoreError != "" || s.EventRuleError != "" || s.DerivedError != "" || s.ProtocolEventError != "" {
		s.Healthy = false
	}
	if e.runtime != nil {
		v := e.runtime.State()
		s.Services = &v
		if v.Phase == "degraded" {
			s.Healthy = false
			s.LastError = v.LastError
		}
	}
	return s
}
func (e *Engine) apply(ctx context.Context, revision uint64, doc config.Document) error {
	if e.runtime != nil {
		if err := e.registerProtocolRelaysLocked(ctx, doc); err != nil {
			return err
		}
		return e.runtime.ApplyConfig(ctx, revision, doc)
	}
	plan, err := config.Compile(doc)
	if err != nil {
		return err
	}
	return e.network.Apply(ctx, revision, plan.Native.Network)
}

// Reconcile deliberately restores only the durable committed revision. A pending
// revision left by a crash is not evidence that its kernel mutations completed.
func (e *Engine) Reconcile(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Confirmation != nil {
		return e.rollbackConfirmation(ctx, "restart")
	}
	if err := e.apply(ctx, e.state.Revision, e.state.Config); err != nil {
		e.state.Healthy = false
		e.state.LastError = err.Error()
		return err
	}
	if err := e.store.ClearPending(); err != nil {
		return err
	}
	e.state.Healthy = true
	e.state.LastError = ""
	e.record("reconciled", "")
	e.committedRuleLifecycleLocked(ctx)
	return nil
}

var ErrConflict = errors.New("configuration revision conflict")
var ErrInvalid = errors.New("invalid configuration request")

func (e *Engine) Commit(ctx context.Context, r CommitRequest) (State, error) {
	if len(r.RequestID) < 1 || len(r.RequestID) > 128 {
		return State{}, fmt.Errorf("%w: request_id must contain 1..128 bytes", ErrInvalid)
	}
	if err := config.Validate(r.Config); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if r.ConfirmTimeoutSeconds != 0 && (r.ConfirmTimeoutSeconds < 5 || r.ConfirmTimeoutSeconds > 3600) {
		return State{}, fmt.Errorf("%w: confirmation timeout must be 5..3600 seconds", ErrInvalid)
	}
	b, _ := json.Marshal(struct {
		Config  config.Document
		Timeout int
	}{r.Config, r.ConfirmTimeoutSeconds})
	hash := sha256.Sum256(b)
	digest := hex.EncodeToString(hash[:])
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.RequestID == e.state.LastRequestID {
		if digest != e.state.LastRequestHash {
			return e.state, fmt.Errorf("%w: request_id reused with different configuration", ErrConflict)
		}
		return e.state, nil
	}
	if e.state.Confirmation != nil {
		return e.state, fmt.Errorf("%w: confirm or roll back the pending revision before another commit", ErrConflict)
	}
	if r.ExpectedRevision != e.state.Revision {
		return e.state, fmt.Errorf("%w: have %d, expected %d", ErrConflict, e.state.Revision, r.ExpectedRevision)
	}
	if e.state.Revision == ^uint64(0) {
		return e.state, errors.New("configuration revision exhausted")
	}
	previous := e.state.Committed
	next := config.Committed{Startup: previous.Startup, StoreID: previous.StoreID, Revision: previous.Revision + 1, Config: r.Config, LastRequestID: r.RequestID, LastRequestHash: digest}
	if r.ConfirmTimeoutSeconds > 0 {
		next.Confirmation = &config.Confirmation{Previous: previous.Config, PreviousRevision: previous.Revision, Deadline: time.Now().Add(time.Duration(r.ConfirmTimeoutSeconds) * time.Second).UTC(), RequestID: r.RequestID}
	}
	reserve := 1
	if next.Confirmation != nil {
		reserve = 2
	}
	next, err := e.withChange(next, "committed", reserve)
	if err != nil {
		return e.state, err
	}
	if err := e.store.Stage(next); err != nil {
		return e.state, fmt.Errorf("stage configuration: %w", err)
	}
	if err := e.apply(ctx, next.Revision, next.Config); err != nil {
		return e.state, e.restore(previous, r.RequestID, fmt.Errorf("apply candidate: %w", err))
	}
	if err := e.store.Save(next); err != nil {
		// An fsync error may follow a successful rename. Rewrite the previous
		// committed record as well as its kernel projection before declaring a rollback.
		return e.state, e.restore(previous, r.RequestID, fmt.Errorf("persist candidate: %w", err))
	}
	e.state.Committed = next
	e.state.Healthy = true
	e.state.LastError = ""
	e.record("committed", r.RequestID)
	e.committedRuleLifecycleLocked(ctx)
	if err := e.store.ClearPending(); err != nil {
		// The committed record is authoritative. Recovery will reconcile it,
		// so stale pending metadata must not cause a successful commit to be repeated.
		e.state.LastError = "committed; pending journal cleanup: " + err.Error()
	}
	return e.state, nil
}

// Confirm makes an already-applied candidate durable across future restarts.
// Revision and request ID tie the acknowledgement to that exact candidate.
func (e *Engine) Confirm(r ConfirmRequest) (State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = e.RefreshEventStore(ctx)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Revision != e.state.Revision || r.RequestID == "" || r.RequestID != e.state.LastRequestID {
		return e.state, fmt.Errorf("%w: confirmation does not identify the current revision", ErrConflict)
	}
	if !e.state.Healthy || e.state.EventStoreError != "" || e.state.EventRuleError != "" || e.state.DerivedError != "" || e.state.ProtocolEventError != "" {
		return e.state, errors.New("cannot confirm while the applied state is degraded")
	}
	if e.runtime != nil && e.runtime.Health().Phase != "ready" {
		return e.state, errors.New("cannot confirm while protocol projection is degraded or applying")
	}
	if e.runtime != nil {
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		err := e.runtime.CheckReady(ctx)
		stop()
		if err != nil {
			return e.state, fmt.Errorf("cannot confirm: %w", err)
		}
	}
	if e.state.Confirmation == nil {
		return e.state, nil
	}
	if !time.Now().Before(e.state.Confirmation.Deadline) {
		return e.state, fmt.Errorf("%w: confirmation deadline expired", ErrConflict)
	}
	next := e.state.Committed
	next.Confirmation = nil
	next, err := e.withChange(next, "confirmed", 1)
	if err != nil {
		return e.state, err
	}
	if err := e.store.Save(next); err != nil {
		e.state.Healthy = false
		e.state.LastError = err.Error()
		return e.state, err
	}
	e.state.Committed = next
	e.record("confirmed", r.RequestID)
	return e.state, nil
}

// Expire runs in the control worker, independent of an SSH session. Failure is
// retried; no new commits/confirmations may hide an unsuccessful rollback.
func (e *Engine) Expire(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Confirmation == nil || now.Before(e.state.Confirmation.Deadline) {
		return nil
	}
	return e.rollbackConfirmation(ctx, "timeout")
}

func (e *Engine) rollbackConfirmation(ctx context.Context, reason string) error {
	p := e.state.Confirmation
	if p == nil {
		return nil
	}
	if e.state.Revision == ^uint64(0) {
		return errors.New("configuration revision exhausted")
	}
	// Rollback is itself a new public revision, so a candidate opened during
	// the trial cannot overwrite the recovered configuration using a stale token.
	next := config.Committed{Startup: e.state.Startup, StoreID: e.state.StoreID, Revision: e.state.Revision + 1, Config: p.Previous}
	next, err := e.withChange(next, "rolled_back_"+reason, 1)
	if err != nil {
		return err
	}
	if err := e.apply(ctx, next.Revision, next.Config); err != nil {
		e.state.Healthy = false
		e.state.LastError = "confirmation rollback: " + err.Error()
		return err
	}
	if err := e.store.Save(next); err != nil {
		e.state.Healthy = false
		e.state.LastError = "persist confirmation rollback: " + err.Error()
		return err
	}
	e.state.Committed = next
	e.state.Healthy = true
	e.state.LastError = ""
	e.record("rolled_back_"+reason, p.RequestID)
	e.committedRuleLifecycleLocked(ctx)
	if err := e.store.ClearPending(); err != nil {
		e.state.LastError = err.Error()
	}
	return nil
}

func (e *Engine) restore(previous config.Committed, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rollbackErr := e.apply(ctx, previous.Revision, previous.Config)
	persistErr := e.store.Save(previous)
	e.state.Healthy = rollbackErr == nil && persistErr == nil
	if e.state.Healthy {
		if err := e.store.ClearPending(); err != nil {
			persistErr = err
			e.state.Healthy = false
		}
	}
	if rollbackErr != nil {
		cause = errors.Join(cause, fmt.Errorf("kernel/protocol rollback failed: %w", rollbackErr))
	}
	if persistErr != nil {
		cause = errors.Join(cause, fmt.Errorf("configuration rollback persistence failed: %w", persistErr))
	}
	e.declareProtocolRelaysLocked()
	e.state.LastError = cause.Error()
	e.record("rejected", id)
	return cause
}

func (e *Engine) record(kind, id string) {
	e.state.Sequence++
	e.events = append(e.events, Event{e.state.ProducerEpoch, e.state.Sequence, e.state.Revision, id, kind})
	if len(e.events) > 256 {
		e.events = append([]Event(nil), e.events[len(e.events)-256:]...)
	}
}

// Changes is bounded and epoch-aware. Consumers take /v1/config again on Reset,
// including after producer restart. Events are hints, never the source of truth.
func (e *Engine) Changes(epoch string, after uint64) Changes {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := Changes{ProducerEpoch: e.state.ProducerEpoch, Sequence: e.state.Sequence, Events: []Event{}}
	if epoch != e.state.ProducerEpoch || after > e.state.Sequence || (len(e.events) > 0 && after < e.events[0].Sequence-1) {
		c.Reset = true
		return c
	}
	for _, event := range e.events {
		if event.Sequence > after {
			c.Events = append(c.Events, event)
		}
	}
	return c
}
