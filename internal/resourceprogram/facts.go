package resourceprogram

import (
	"errors"
	"fmt"
	"sync"
)

// Truth distinguishes an observed false condition from missing or stale data.
// An indeterminate guard must never authorize a resource to start.
type Truth uint8

const (
	Indeterminate Truth = iota
	False
	True
)

func (t Truth) Not() Truth {
	switch t {
	case True:
		return False
	case False:
		return True
	default:
		return Indeterminate
	}
}

// All and Any use three-valued logic: false dominates conjunction and true
// dominates disjunction, even if another input is indeterminate.
func All(values ...Truth) Truth {
	if len(values) == 0 {
		return Indeterminate
	}
	result := True
	for _, v := range values {
		if v == False {
			return False
		}
		if v != True {
			result = Indeterminate
		}
	}
	return result
}

func Any(values ...Truth) Truth {
	if len(values) == 0 {
		return Indeterminate
	}
	result := False
	for _, v := range values {
		if v == True {
			return True
		}
		if v != False {
			result = Indeterminate
		}
	}
	return result
}

// FactScope is the exact runtime projection within one Linux boot. A change
// of boot, source digest, operator revision, or projection generation revokes
// all facts; it does not alter the operator's stored source program.
type FactScope struct {
	BootID               string
	ConfigDigest         string
	OperatorRevision     uint64
	ProjectionGeneration uint64
}

// ProducerEpoch identifies the supervised worker producing a fact. Epoch must
// advance on producer replacement; WorkerGeneration distinguishes instances.
type ProducerEpoch struct {
	Epoch            uint64
	WorkerGeneration uint64
}

// ProtocolFact is a transient observation, not a durable event or a lease
// authorizing side effects. Times are nanoseconds of CLOCK_BOOTTIME supplied
// by the caller; wall-clock times must not be used. ExpiresNanos is mandatory.
// Sequence increases across every fact emitted by the same producer instance.
type ProtocolFact struct {
	Key           string
	Producer      string
	Scope         FactScope
	Epoch         ProducerEpoch
	Sequence      uint64
	ObservedNanos uint64
	ExpiresNanos  uint64
	Value         bool
}

type producerFactState struct {
	Epoch    ProducerEpoch
	Sequence uint64
	Active   bool
}

// FactStore holds only the currently valid observations. The supervisor owns
// the activation and revocation calls; untrusted worker messages can Publish
// but cannot register their own producer epoch.
type FactStore struct {
	mu        sync.RWMutex
	scope     FactScope
	producers map[string]producerFactState
	facts     map[string]ProtocolFact
}

func validFactScope(s FactScope) bool {
	return s.BootID != "" && s.ConfigDigest != ""
}

func NewFactStore(scope FactScope) (*FactStore, error) {
	if !validFactScope(scope) {
		return nil, errors.New("fact scope requires boot ID and config digest")
	}
	return &FactStore{scope: scope, producers: make(map[string]producerFactState), facts: make(map[string]ProtocolFact)}, nil
}

// ReplaceFactScope revokes all producer registrations before a new projection.
// It is idempotent for an identical scope. No old Up can survive a cutover.
func (s *FactStore) ReplaceFactScope(scope FactScope) error {
	if !validFactScope(scope) {
		return errors.New("fact scope requires boot ID and config digest")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope != scope {
		s.scope = scope
		s.producers = make(map[string]producerFactState)
		s.facts = make(map[string]ProtocolFact)
	}
	return nil
}

func laterProducer(a, b ProducerEpoch) bool {
	return a.Epoch > b.Epoch || (a.Epoch == b.Epoch && a.WorkerGeneration > b.WorkerGeneration)
}

// ActivateProducer must be called by the supervisor with the current scope.
// An identical active registration is idempotent; a revoked or older epoch
// cannot reactivate stale facts or reuse a message sequence.
func (s *FactStore) ActivateProducer(scope FactScope, name string, epoch ProducerEpoch) error {
	if name == "" || epoch.Epoch == 0 || epoch.WorkerGeneration == 0 {
		return errors.New("invalid producer identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope != scope {
		return errors.New("producer scope is obsolete")
	}
	if old, ok := s.producers[name]; ok {
		if old.Epoch == epoch && old.Active {
			return nil
		}
		if !laterProducer(epoch, old.Epoch) {
			return errors.New("producer epoch must advance")
		}
	}
	for key, fact := range s.facts {
		if fact.Producer == name {
			delete(s.facts, key)
		}
	}
	s.producers[name] = producerFactState{Epoch: epoch, Active: true}
	return nil
}

// RevokeProducer retains the epoch tombstone to reject delayed reactivation.
func (s *FactStore) RevokeProducer(scope FactScope, name string, epoch ProducerEpoch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.producers[name]
	if s.scope != scope || !ok || state.Epoch != epoch || !state.Active {
		return errors.New("producer revocation is obsolete")
	}
	state.Active = false
	s.producers[name] = state
	for key, fact := range s.facts {
		if fact.Producer == name {
			delete(s.facts, key)
		}
	}
	return nil
}

// Publish rejects duplicate/out-of-order messages, non-finite or expired facts,
// and any notification from a previous boot, projection or worker instance.
// Failed publications never advance the sequence watermark.
func (s *FactStore) Publish(f ProtocolFact, nowNanos uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.producers[f.Producer]
	if f.Key == "" || f.Producer == "" || !ok || !state.Active || state.Epoch != f.Epoch || f.Scope != s.scope {
		return errors.New("fact is not from the active producer and projection")
	}
	if f.Sequence == 0 || f.Sequence <= state.Sequence {
		return errors.New("fact sequence is duplicated or out of order")
	}
	if f.ExpiresNanos == 0 || f.ExpiresNanos <= f.ObservedNanos || f.ObservedNanos > nowNanos || f.ExpiresNanos <= nowNanos {
		return errors.New("fact must have a finite, current CLOCK_BOOTTIME interval")
	}
	if old, exists := s.facts[f.Key]; exists && old.Producer != f.Producer && old.ExpiresNanos > nowNanos {
		return fmt.Errorf("fact %q is owned by active producer %q", f.Key, old.Producer)
	}
	state.Sequence = f.Sequence
	s.producers[f.Producer] = state
	s.facts[f.Key] = f
	return nil
}

// FactValue is indeterminate when a fact is absent, expired or revoked.
// In particular it never treats an absent VRRP role or health status as false.
func (s *FactStore) FactValue(key string, nowNanos uint64) Truth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fact, ok := s.facts[key]
	if !ok || nowNanos < fact.ObservedNanos || nowNanos >= fact.ExpiresNanos || fact.Scope != s.scope {
		return Indeterminate
	}
	state, ok := s.producers[fact.Producer]
	if !ok || !state.Active || state.Epoch != fact.Epoch {
		return Indeterminate
	}
	if fact.Value {
		return True
	}
	return False
}
